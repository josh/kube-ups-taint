package main

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
)

const prefix = "kube-ups-taint.josh.github.io/"

const (
	statusKey            = prefix + "status"
	batteryChargeKey     = prefix + "battery-charge"
	batteryRuntimeKey    = prefix + "battery-runtime"
	batteryBelowPrefix   = prefix + "battery-below-"
	runtimeBelowPrefix   = prefix + "runtime-below-"
	upsKey               = prefix + "ups"
	upsStatusAnnotation  = prefix + "ups-status"
	observedAtAnnotation = prefix + "observed-at"
)

const secretsDir = "/var/run/secrets/nut"

type rawUPS struct {
	Name         string `json:"name"`
	Address      string `json:"address"`
	NodeSelector string `json:"nodeSelector,omitempty"`
}

type rawConfig struct {
	Debug                  *bool    `json:"debug,omitempty"`
	Interval               string   `json:"interval,omitempty"`
	StaleAfter             string   `json:"staleAfter,omitempty"`
	MaxConsecutiveFailures *int     `json:"maxConsecutiveFailures,omitempty"`
	BatterySteps           *[]int   `json:"batterySteps,omitempty"`
	RuntimeSteps           *[]int   `json:"runtimeSteps,omitempty"`
	BatteryChargeTaint     *bool    `json:"batteryChargeTaint,omitempty"`
	BatteryRuntimeTaint    *bool    `json:"batteryRuntimeTaint,omitempty"`
	UPS                    []rawUPS `json:"ups,omitempty"`
}

const defaultInterval = 15 * time.Second

const defaultStaleAfter = 2 * time.Minute

const defaultMaxConsecutiveFailures = 10

var defaultBatterySteps = []int{50}

type upsConfig struct {
	name     string
	address  upsAddress
	selector labels.Selector
	username string
	password string
}

type config struct {
	debug               bool
	interval            time.Duration
	staleAfter          time.Duration
	maxFailures         int
	batterySteps        []int
	runtimeSteps        []int
	batteryChargeTaint  bool
	batteryRuntimeTaint bool
	ups                 []upsConfig
}

func (c config) LogValue() slog.Value {
	var ups []string
	for _, u := range c.ups {
		auth := ""
		if u.username != "" {
			auth = " user=" + u.username + " password=[redacted]"
		}
		ups = append(ups, fmt.Sprintf("%s=%s selector=%q%s", u.name, u.address, u.selector.String(), auth))
	}
	return slog.GroupValue(
		slog.Bool("debug", c.debug),
		slog.Duration("interval", c.interval),
		slog.Duration("staleAfter", c.staleAfter),
		slog.Int("maxConsecutiveFailures", c.maxFailures),
		slog.Any("batterySteps", c.batterySteps),
		slog.Any("runtimeSteps", c.runtimeSteps),
		slog.Bool("batteryChargeTaint", c.batteryChargeTaint),
		slog.Bool("batteryRuntimeTaint", c.batteryRuntimeTaint),
		slog.Any("ups", ups),
	)
}

func parseDuration(name, value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s in config: %w", name, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive: %s", name, value)
	}
	return d, nil
}

func parseSteps(name string, steps *[]int, fallback []int, upper int) ([]int, error) {
	if steps == nil {
		return slices.Clone(fallback), nil
	}
	sorted := slices.Clone(*steps)
	slices.Sort(sorted)
	for i, s := range sorted {
		if s < 1 || (upper > 0 && s > upper) {
			return nil, fmt.Errorf("%s value out of range: %d", name, s)
		}
		if i > 0 && sorted[i-1] == s {
			return nil, fmt.Errorf("%s contains duplicate value: %d", name, s)
		}
	}
	return sorted, nil
}

func readSecret(name, key string) string {
	data, err := os.ReadFile(filepath.Join(secretsDir, name, key))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func loadConfig() (config, error) {
	path := "/etc/kube-ups-taint/config.json"
	if v := os.Getenv("KUBE_UPS_TAINT_CONFIG_PATH"); v != "" {
		path = v
	}
	var raw rawConfig
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return config{}, fmt.Errorf("open config file: %w", err)
		}
	} else {
		defer func() { _ = f.Close() }()
		if err := json.NewDecoder(f).Decode(&raw); err != nil {
			return config{}, fmt.Errorf("decode config file: %w", err)
		}
	}

	interval, err := parseDuration("interval", raw.Interval, defaultInterval)
	if err != nil {
		return config{}, err
	}
	staleAfter, err := parseDuration("staleAfter", raw.StaleAfter, defaultStaleAfter)
	if err != nil {
		return config{}, err
	}
	maxFailures := defaultMaxConsecutiveFailures
	if raw.MaxConsecutiveFailures != nil {
		if *raw.MaxConsecutiveFailures < 1 {
			return config{}, fmt.Errorf("maxConsecutiveFailures must be positive: %d", *raw.MaxConsecutiveFailures)
		}
		maxFailures = *raw.MaxConsecutiveFailures
	}
	batterySteps, err := parseSteps("batterySteps", raw.BatterySteps, defaultBatterySteps, 100)
	if err != nil {
		return config{}, err
	}
	runtimeSteps, err := parseSteps("runtimeSteps", raw.RuntimeSteps, nil, 0)
	if err != nil {
		return config{}, err
	}

	var ups []upsConfig
	seen := map[string]bool{}
	for _, r := range raw.UPS {
		if r.Name == "" {
			return config{}, fmt.Errorf("ups entry is missing a name")
		}
		if errs := validation.IsValidLabelValue(r.Name); len(errs) > 0 {
			return config{}, fmt.Errorf("invalid ups name %q: %s", r.Name, strings.Join(errs, "; "))
		}
		if seen[r.Name] {
			return config{}, fmt.Errorf("duplicate ups name: %s", r.Name)
		}
		seen[r.Name] = true
		addr, err := parseUPSAddress(r.Address)
		if err != nil {
			return config{}, fmt.Errorf("ups %s: %w", r.Name, err)
		}
		selector := labels.SelectorFromSet(labels.Set{upsKey: r.Name})
		if r.NodeSelector != "" {
			selector, err = labels.Parse(r.NodeSelector)
			if err != nil {
				return config{}, fmt.Errorf("ups %s: invalid nodeSelector: %w", r.Name, err)
			}
		}
		ups = append(ups, upsConfig{
			name:     r.Name,
			address:  addr,
			selector: selector,
			username: readSecret(r.Name, "username"),
			password: readSecret(r.Name, "password"),
		})
	}

	return config{
		debug:               raw.Debug != nil && *raw.Debug,
		interval:            interval,
		staleAfter:          staleAfter,
		maxFailures:         maxFailures,
		batterySteps:        batterySteps,
		runtimeSteps:        runtimeSteps,
		batteryChargeTaint:  raw.BatteryChargeTaint != nil && *raw.BatteryChargeTaint,
		batteryRuntimeTaint: raw.BatteryRuntimeTaint != nil && *raw.BatteryRuntimeTaint,
		ups:                 ups,
	}, nil
}

var version = "0.0.0"

const fieldManager = "kube-ups-taint"

var startedAt = time.Now()

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("kube-ups-taint: %s\n", version)
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if cfg.debug {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}
	slog.Debug("loaded config", "config", cfg)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	clientset, err := getKubeClient()
	if err != nil {
		slog.Error("failed to connect to kubernetes", "error", err)
		os.Exit(1)
	}

	consecutiveFailures := 0
	tick := func() {
		if err := run(ctx, cfg, clientset); err != nil {
			if ctx.Err() != nil {
				return
			}
			consecutiveFailures++
			slog.Error("run failed", "error", err, "consecutiveFailures", consecutiveFailures)
			if consecutiveFailures >= cfg.maxFailures {
				slog.Error("too many consecutive failures, exiting", "failures", consecutiveFailures)
				os.Exit(1)
			}
		} else {
			consecutiveFailures = 0
		}
	}

	tick()

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			newCfg, err := loadConfig()
			if err != nil {
				slog.Error("failed to reload config, using previous configuration", "error", err)
			} else if !reflect.DeepEqual(cfg, newCfg) {
				slog.Debug("configuration changed", "from", cfg, "to", newCfg)
				if newCfg.debug != cfg.debug {
					slog.Info("log level changed", "debug", newCfg.debug)
					if newCfg.debug {
						slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
					} else {
						slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{})))
					}
				}
				if newCfg.interval != cfg.interval {
					ticker.Reset(newCfg.interval)
					slog.Info("interval changed", "interval", newCfg.interval)
				}
				cfg = newCfg
			}
			tick()
		}
	}
}

func getKubeClient() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}

	return clientset, nil
}

type reading struct {
	status     string
	charge     int
	hasCharge  bool
	runtime    int
	hasRuntime bool
}

func (r *reading) online() bool {
	return slices.Contains(strings.Fields(r.status), "OL")
}

func readUPS(ctx context.Context, u upsConfig) (*reading, error) {
	c, err := dialNUT(ctx, u.address.host)
	if err != nil {
		return nil, err
	}
	defer c.close()

	if u.username != "" {
		if err := c.login(u.username, u.password); err != nil {
			return nil, fmt.Errorf("login: %w", err)
		}
	}

	status, err := c.getVar(u.address.ups, "ups.status")
	if err != nil {
		return nil, fmt.Errorf("get ups.status: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		return nil, fmt.Errorf("empty ups.status")
	}
	r := &reading{status: status}

	r.charge, r.hasCharge, err = getLevel(c, u.address.ups, "battery.charge")
	if err != nil {
		return nil, err
	}
	r.charge = min(r.charge, 100)

	r.runtime, r.hasRuntime, err = getLevel(c, u.address.ups, "battery.runtime")
	if err != nil {
		return nil, err
	}

	return r, nil
}

func getLevel(c *nutClient, ups, name string) (int, bool, error) {
	v, err := c.getVar(ups, name)
	if stderrors.Is(err, errVarNotSupported) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("get %s: %w", name, err)
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse %s %q: %w", name, v, err)
	}
	return max(int(math.Round(f)), 0), true, nil
}

var lastStatus = map[string]string{}

func run(ctx context.Context, cfg config, clientset *kubernetes.Clientset) error {
	var errs []error

	readings := map[string]*reading{}
	for _, u := range cfg.ups {
		r, err := readUPS(ctx, u)
		if err != nil {
			slog.Error("failed to read ups", "ups", u.name, "address", u.address.String(), "error", err)
			continue
		}
		slog.Debug("read ups", "ups", u.name, "status", r.status, "charge", r.charge, "runtime", r.runtime)
		if lastStatus[u.name] != r.status {
			slog.Info("ups status changed", "ups", u.name, "status", r.status, "charge", r.charge, "runtime", r.runtime)
			lastStatus[u.name] = r.status
		}
		readings[u.name] = r
	}

	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return stderrors.Join(append(errs, fmt.Errorf("list nodes: %w", err))...)
	}

	for _, node := range nodes.Items {
		var matched []upsConfig
		for _, u := range cfg.ups {
			if u.selector.Matches(labels.Set(node.Labels)) {
				matched = append(matched, u)
			}
		}
		if len(matched) == 0 {
			if len(cfg.ups) > 0 && isManaged(&node) {
				if err := releaseNode(ctx, clientset, node.Name); err != nil {
					errs = append(errs, fmt.Errorf("release node %s: %w", node.Name, err))
				}
			}
			continue
		}
		if len(matched) > 1 {
			var names []string
			for _, u := range matched {
				names = append(names, u.name)
			}
			slog.Error("node matches multiple ups entries, skipping", "node", node.Name, "ups", strings.Join(names, ", "))
			continue
		}
		if err := syncNode(ctx, cfg, clientset, node.Name, matched[0], readings[matched[0].name]); err != nil {
			errs = append(errs, fmt.Errorf("sync node %s: %w", node.Name, err))
		}
	}

	return stderrors.Join(errs...)
}

func syncNode(ctx context.Context, cfg config, clientset *kubernetes.Clientset, nodeName string, u upsConfig, r *reading) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get node: %w", err)
		}

		now := time.Now()
		existing := ownedTaints(node.Spec.Taints)
		desired, stale := desiredTaints(cfg, existing, node.Annotations, r, now)
		annotations := desiredAnnotations(cfg, node.Annotations, u, r, now)

		taintsChanged := !sameTaints(existing, desired)
		if !taintsChanged && maps.Equal(annotations, node.Annotations) {
			slog.Debug("node already up-to-date", "node", nodeName, "ups", u.name)
			return nil
		}

		node.Spec.Taints = mergeTaints(node.Spec.Taints, desired, now)
		node.Annotations = annotations
		if _, err := clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
			return err
		}

		if !taintsChanged {
			slog.Debug("updated node annotations", "node", nodeName, "ups", u.name)
			return nil
		}
		slog.Info("updated node taints", "node", nodeName, "ups", u.name, "taints", formatTaints(desired))
		recordEvent(ctx, clientset, node, u, r, stale, desired)
		return nil
	})
}

func isManaged(node *corev1.Node) bool {
	if len(ownedTaints(node.Spec.Taints)) > 0 {
		return true
	}
	for key := range node.Annotations {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func releaseNode(ctx context.Context, clientset *kubernetes.Clientset, nodeName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get node: %w", err)
		}
		if !isManaged(node) {
			return nil
		}

		removed := ownedTaints(node.Spec.Taints)
		node.Spec.Taints = mergeTaints(node.Spec.Taints, nil, time.Now())
		maps.DeleteFunc(node.Annotations, func(key, _ string) bool { return strings.HasPrefix(key, prefix) })
		if _, err := clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
			return err
		}

		slog.Info("released node no longer mapped to a ups", "node", nodeName, "removedTaints", formatTaints(removed))
		return nil
	})
}

func ownedTaints(taints []corev1.Taint) []corev1.Taint {
	var owned []corev1.Taint
	for _, t := range taints {
		if strings.HasPrefix(t.Key, prefix) {
			owned = append(owned, t)
		}
	}
	return owned
}

func findTaint(taints []corev1.Taint, key string) *corev1.Taint {
	for i := range taints {
		if taints[i].Key == key {
			return &taints[i]
		}
	}
	return nil
}

func desiredTaints(cfg config, existing []corev1.Taint, annotations map[string]string, r *reading, now time.Time) ([]corev1.Taint, bool) {
	if r == nil {
		observed := startedAt
		if t, err := time.Parse(time.RFC3339, annotations[observedAtAnnotation]); err == nil {
			observed = t
		}
		if now.Sub(observed) <= cfg.staleAfter {
			return existing, false
		}
		desired := slices.DeleteFunc(slices.Clone(existing), func(t corev1.Taint) bool { return t.Key == statusKey })
		desired = append(desired, corev1.Taint{Key: statusKey, Value: "unknown", Effect: corev1.TaintEffectNoSchedule})
		return desired, true
	}

	if r.online() {
		return nil, false
	}

	var desired []corev1.Taint
	flags := strings.Fields(r.status)
	switch {
	case slices.Contains(flags, "OB") && slices.Contains(flags, "LB"):
		desired = append(desired, corev1.Taint{Key: statusKey, Value: "low-battery", Effect: corev1.TaintEffectNoExecute})
	case slices.Contains(flags, "OB"):
		desired = append(desired, corev1.Taint{Key: statusKey, Value: "on-battery", Effect: corev1.TaintEffectNoSchedule})
	default:
		desired = append(desired, corev1.Taint{Key: statusKey, Value: "unknown", Effect: corev1.TaintEffectNoSchedule})
	}

	for _, s := range cfg.batterySteps {
		key := batteryBelowPrefix + strconv.Itoa(s)
		if (r.hasCharge && r.charge < s) || findTaint(existing, key) != nil {
			desired = append(desired, corev1.Taint{Key: key, Effect: corev1.TaintEffectNoExecute})
		}
	}
	for _, s := range cfg.runtimeSteps {
		key := runtimeBelowPrefix + strconv.Itoa(s)
		if (r.hasRuntime && r.runtime < s) || findTaint(existing, key) != nil {
			desired = append(desired, corev1.Taint{Key: key, Effect: corev1.TaintEffectNoExecute})
		}
	}
	if cfg.batteryChargeTaint {
		if t, ok := ratchetLevel(existing, batteryChargeKey, r.charge, r.hasCharge); ok {
			desired = append(desired, t)
		}
	}
	if cfg.batteryRuntimeTaint {
		if t, ok := ratchetLevel(existing, batteryRuntimeKey, r.runtime, r.hasRuntime); ok {
			desired = append(desired, t)
		}
	}
	return desired, false
}

func ratchetLevel(existing []corev1.Taint, key string, value int, ok bool) (corev1.Taint, bool) {
	if prev := findTaint(existing, key); prev != nil {
		if p, err := strconv.Atoi(prev.Value); err == nil && (!ok || p < value) {
			value, ok = p, true
		}
	}
	if !ok {
		return corev1.Taint{}, false
	}
	return corev1.Taint{Key: key, Value: strconv.Itoa(value), Effect: corev1.TaintEffectNoExecute}, true
}

func sameTaints(a, b []corev1.Taint) bool {
	if len(a) != len(b) {
		return false
	}
	for _, t := range a {
		if !slices.ContainsFunc(b, func(o corev1.Taint) bool {
			return o.Key == t.Key && o.Value == t.Value && o.Effect == t.Effect
		}) {
			return false
		}
	}
	return true
}

func mergeTaints(taints, desired []corev1.Taint, now time.Time) []corev1.Taint {
	var merged []corev1.Taint
	for _, t := range taints {
		if !strings.HasPrefix(t.Key, prefix) {
			merged = append(merged, t)
		}
	}
	for _, t := range desired {
		if prev := findTaint(taints, t.Key); prev != nil && prev.Value == t.Value && prev.Effect == t.Effect {
			t.TimeAdded = prev.TimeAdded
		}
		if t.TimeAdded == nil && t.Effect == corev1.TaintEffectNoExecute {
			t.TimeAdded = &metav1.Time{Time: now}
		}
		merged = append(merged, t)
	}
	return merged
}

func desiredAnnotations(cfg config, current map[string]string, u upsConfig, r *reading, now time.Time) map[string]string {
	annotations := maps.Clone(current)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[upsKey] = u.name
	if r == nil {
		return annotations
	}

	set := func(key, value string, ok bool) {
		if ok {
			annotations[key] = value
		} else {
			delete(annotations, key)
		}
	}
	set(upsStatusAnnotation, r.status, true)
	set(batteryChargeKey, strconv.Itoa(r.charge), r.hasCharge)
	set(batteryRuntimeKey, strconv.Itoa(r.runtime), r.hasRuntime)

	observed, err := time.Parse(time.RFC3339, current[observedAtAnnotation])
	readingChanged := !maps.Equal(withoutKey(annotations, observedAtAnnotation), withoutKey(current, observedAtAnnotation))
	if err != nil || readingChanged || now.Sub(observed) > cfg.staleAfter/4 {
		annotations[observedAtAnnotation] = now.UTC().Format(time.RFC3339)
	}
	return annotations
}

func withoutKey(m map[string]string, key string) map[string]string {
	m = maps.Clone(m)
	delete(m, key)
	return m
}

func formatTaints(taints []corev1.Taint) string {
	if len(taints) == 0 {
		return "none"
	}
	var parts []string
	for _, t := range taints {
		parts = append(parts, t.ToString())
	}
	return strings.Join(parts, ", ")
}

func recordEvent(ctx context.Context, clientset *kubernetes.Clientset, node *corev1.Node, u upsConfig, r *reading, stale bool, desired []corev1.Taint) {
	eventType, reason := corev1.EventTypeWarning, "BatteryLevel"
	status := findTaint(desired, statusKey)
	switch {
	case stale:
		reason = "UPSUnreachable"
	case len(desired) == 0:
		eventType, reason = corev1.EventTypeNormal, "Online"
	case status != nil && status.Value == "low-battery":
		reason = "LowBattery"
	case status != nil && status.Value == "on-battery":
		reason = "OnBattery"
	}

	message := fmt.Sprintf("UPS %s (%s) unreachable; taints: %s", u.name, u.address, formatTaints(desired))
	if r != nil {
		message = fmt.Sprintf("UPS %s (%s) status %q, charge %s, runtime %s; taints: %s", u.name, u.address, r.status, levelString(r.charge, r.hasCharge, "%"), levelString(r.runtime, r.hasRuntime, "s"), formatTaints(desired))
	}

	instance, _ := os.Hostname()
	now := metav1.Now()
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s.%x", node.Name, now.UnixNano()),
			Namespace: metav1.NamespaceDefault,
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Node",
			Name:       node.Name,
			UID:        node.UID,
		},
		Reason:              reason,
		Message:             message,
		Type:                eventType,
		Source:              corev1.EventSource{Component: fieldManager},
		FirstTimestamp:      now,
		LastTimestamp:       now,
		Count:               1,
		ReportingController: fieldManager,
		ReportingInstance:   instance,
	}
	if _, err := clientset.CoreV1().Events(metav1.NamespaceDefault).Create(ctx, event, metav1.CreateOptions{}); err != nil {
		slog.Warn("failed to record event", "node", node.Name, "reason", reason, "error", err)
	}
}

func levelString(v int, ok bool, unit string) string {
	if !ok {
		return "unknown"
	}
	return strconv.Itoa(v) + unit
}
