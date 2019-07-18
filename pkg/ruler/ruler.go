package ruler

import (
	native_ctx "context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/notifier"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/rules"
	promStorage "github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/strutil"
	"golang.org/x/net/context"
	"golang.org/x/net/context/ctxhttp"

	"github.com/cortexproject/cortex/pkg/distributor"
	"github.com/cortexproject/cortex/pkg/ring"
	"github.com/cortexproject/cortex/pkg/ruler/store"
	"github.com/cortexproject/cortex/pkg/util"
	"github.com/cortexproject/cortex/pkg/util/flagext"
	"github.com/weaveworks/common/instrument"
	"github.com/weaveworks/common/user"
)

var (
	evalDuration = instrument.NewHistogramCollectorFromOpts(prometheus.HistogramOpts{
		Namespace: "cortex",
		Name:      "group_evaluation_duration_seconds",
		Help:      "The duration for a rule group to execute.",
		Buckets:   []float64{.5, 1, 2.5, 5, 10, 25, 60, 120},
	})
	ringCheckErrors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "cortex",
		Name:      "ruler_ring_check_errors_total",
		Help:      "Number of errors that have occurred when checking the ring for ownership",
	})
)

func init() {
	evalDuration.Register()
}

// Config is the configuration for the recording rules server.
type Config struct {
	// This is used for template expansion in alerts; must be a valid URL
	ExternalURL flagext.URLValue

	// How frequently to evaluate rules by default.
	EvaluationInterval time.Duration
	NumWorkers         int

	// URL of the Alertmanager to send notifications to.
	AlertmanagerURL flagext.URLValue
	// Whether to use DNS SRV records to discover alertmanagers.
	AlertmanagerDiscovery bool
	// How long to wait between refreshing the list of alertmanagers based on
	// DNS service discovery.
	AlertmanagerRefreshInterval time.Duration

	// Capacity of the queue for notifications to be sent to the Alertmanager.
	NotificationQueueCapacity int
	// HTTP timeout duration when sending notifications to the Alertmanager.
	NotificationTimeout time.Duration
	// Timeout for rule group evaluation, including sending result to ingester
	GroupTimeout time.Duration

	EnableSharding bool

	SearchPendingFor time.Duration
	LifecyclerConfig ring.LifecyclerConfig
	FlushCheckPeriod time.Duration

	StoreConfig RuleStoreConfig
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	cfg.LifecyclerConfig.RegisterFlagsWithPrefix("ruler.", f)
	cfg.StoreConfig.RegisterFlags(f)

	cfg.ExternalURL.URL, _ = url.Parse("") // Must be non-nil
	f.Var(&cfg.ExternalURL, "ruler.external.url", "URL of alerts return path.")
	f.DurationVar(&cfg.EvaluationInterval, "ruler.evaluation-interval", 15*time.Second, "How frequently to evaluate rules")
	f.IntVar(&cfg.NumWorkers, "ruler.num-workers", 1, "Number of rule evaluator worker routines in this process")
	f.Var(&cfg.AlertmanagerURL, "ruler.alertmanager-url", "URL of the Alertmanager to send notifications to.")
	f.BoolVar(&cfg.AlertmanagerDiscovery, "ruler.alertmanager-discovery", false, "Use DNS SRV records to discover alertmanager hosts.")
	f.DurationVar(&cfg.AlertmanagerRefreshInterval, "ruler.alertmanager-refresh-interval", 1*time.Minute, "How long to wait between refreshing alertmanager hosts.")
	f.IntVar(&cfg.NotificationQueueCapacity, "ruler.notification-queue-capacity", 10000, "Capacity of the queue for notifications to be sent to the Alertmanager.")
	f.DurationVar(&cfg.NotificationTimeout, "ruler.notification-timeout", 10*time.Second, "HTTP timeout duration when sending notifications to the Alertmanager.")
	f.DurationVar(&cfg.GroupTimeout, "ruler.group-timeout", 10*time.Second, "Timeout for rule group evaluation, including sending result to ingester")
	if flag.Lookup("promql.lookback-delta") == nil {
		flag.DurationVar(&promql.LookbackDelta, "promql.lookback-delta", promql.LookbackDelta, "Time since the last sample after which a time series is considered stale and ignored by expression evaluations.")
	}
	f.DurationVar(&cfg.SearchPendingFor, "ruler.search-pending-for", 5*time.Minute, "Time to spend searching for a pending ruler when shutting down.")
	f.BoolVar(&cfg.EnableSharding, "ruler.enable-sharding", false, "Distribute rule evaluation using ring backend")
	f.DurationVar(&cfg.FlushCheckPeriod, "ruler.flush-period", 1*time.Minute, "Period with which to attempt to flush rule groups.")
}

// Ruler evaluates rules.
type Ruler struct {
	cfg         Config
	engine      *promql.Engine
	queryable   promStorage.Queryable
	pusher      Pusher
	alertURL    *url.URL
	notifierCfg *config.Config

	scheduler *scheduler
	workerWG  *sync.WaitGroup

	lifecycler *ring.Lifecycler
	ring       *ring.Ring

	store store.RuleStore

	// Per-user notifiers with separate queues.
	notifiersMtx sync.Mutex
	notifiers    map[string]*rulerNotifier

	// Per-user rules metrics
	userMetricsMtx sync.Mutex
	userMetrics    map[string]*rules.Metrics
}

// NewRuler creates a new ruler from a distributor and chunk store.
func NewRuler(cfg Config, engine *promql.Engine, queryable promStorage.Queryable, d *distributor.Distributor) (*Ruler, error) {
	if cfg.NumWorkers <= 0 {
		return nil, fmt.Errorf("must have at least 1 worker, got %d", cfg.NumWorkers)
	}

	ncfg, err := buildNotifierConfig(&cfg)
	if err != nil {
		return nil, err
	}

	rulePoller, ruleStore, err := NewRuleStorage(cfg.StoreConfig)

	ruler := &Ruler{
		cfg:         cfg,
		engine:      engine,
		queryable:   queryable,
		pusher:      d,
		alertURL:    cfg.ExternalURL.URL,
		notifierCfg: ncfg,
		notifiers:   map[string]*rulerNotifier{},
		workerWG:    &sync.WaitGroup{},
		userMetrics: map[string]*rules.Metrics{},
		store:       ruleStore,
	}

	ruler.scheduler = newScheduler(rulePoller, cfg.EvaluationInterval, cfg.EvaluationInterval, ruler.newGroup)

	// If sharding is enabled, create/join a ring to distribute tokens to
	// the ruler
	if cfg.EnableSharding {
		ruler.lifecycler, err = ring.NewLifecycler(cfg.LifecyclerConfig, ruler, "ruler")
		if err != nil {
			return nil, err
		}

		ruler.ring, err = ring.New(cfg.LifecyclerConfig.RingConfig, "ruler")
		if err != nil {
			return nil, err
		}
	}

	for i := 0; i < cfg.NumWorkers; i++ {
		// initialize each worker in a function that signals when
		// the worker has completed
		ruler.workerWG.Add(1)
		go func() {
			w := newWorker(ruler)
			w.Run()
			ruler.workerWG.Done()
		}()
	}

	go ruler.scheduler.Run()

	level.Info(util.Logger).Log("msg", "ruler up and running")

	return ruler, nil
}

// Stop stops the Ruler.
// Each function of the ruler is terminated before leaving the ring
func (r *Ruler) Stop() {
	r.notifiersMtx.Lock()
	for _, n := range r.notifiers {
		n.stop()
	}
	r.notifiersMtx.Unlock()

	level.Info(util.Logger).Log("msg", "shutting down rules scheduler")
	r.scheduler.Stop()

	level.Info(util.Logger).Log("msg", "waiting for workers to finish")
	r.workerWG.Wait()

	if r.cfg.EnableSharding {
		level.Info(util.Logger).Log("msg", "attempting shutdown lifecycle")
		r.lifecycler.Shutdown()
		level.Info(util.Logger).Log("msg", "shutting down the ring")
		r.ring.Stop()
	}
}

func (r *Ruler) newGroup(ctx context.Context, g store.RuleGroup) (*wrappedGroup, error) {
	user := g.User()
	appendable := &appendableAppender{pusher: r.pusher}
	notifier, err := r.getOrCreateNotifier(user)
	if err != nil {
		return nil, err
	}

	rls, err := g.Rules(ctx)
	if err != nil {
		return nil, err
	}

	// Get the rule group metrics for set user or create it if it does not exist
	r.userMetricsMtx.Lock()
	metrics, exists := r.userMetrics[user]
	if !exists {
		// Wrap the default register with the users ID and pass
		reg := prometheus.WrapRegistererWith(prometheus.Labels{"user": user}, prometheus.DefaultRegisterer)
		metrics = rules.NewGroupMetrics(reg)
		r.userMetrics[user] = metrics
	}
	r.userMetricsMtx.Unlock()

	opts := &rules.ManagerOptions{
		Appendable:  appendable,
		QueryFunc:   rules.EngineQueryFunc(r.engine, r.queryable),
		Context:     context.Background(),
		ExternalURL: r.alertURL,
		NotifyFunc:  sendAlerts(notifier, r.alertURL.String()),
		Logger:      util.Logger,
		Metrics:     metrics,
	}
	return newGroup(g.ID(), rls, appendable, opts), nil
}

// sendAlerts implements a rules.NotifyFunc for a Notifier.
// It filters any non-firing alerts from the input.
//
// Copied from Prometheus's main.go.
func sendAlerts(n *notifier.Manager, externalURL string) rules.NotifyFunc {
	return func(ctx native_ctx.Context, expr string, alerts ...*rules.Alert) {
		var res []*notifier.Alert

		for _, alert := range alerts {
			// Only send actually firing alerts.
			if alert.State == rules.StatePending {
				continue
			}
			a := &notifier.Alert{
				StartsAt:     alert.FiredAt,
				Labels:       alert.Labels,
				Annotations:  alert.Annotations,
				GeneratorURL: externalURL + strutil.TableLinkForExpression(expr),
			}
			if !alert.ResolvedAt.IsZero() {
				a.EndsAt = alert.ResolvedAt
			}
			res = append(res, a)
		}

		if len(alerts) > 0 {
			n.Send(res...)
		}
	}
}

func (r *Ruler) getOrCreateNotifier(userID string) (*notifier.Manager, error) {
	r.notifiersMtx.Lock()
	defer r.notifiersMtx.Unlock()

	n, ok := r.notifiers[userID]
	if ok {
		return n.notifier, nil
	}

	n = newRulerNotifier(&notifier.Options{
		QueueCapacity: r.cfg.NotificationQueueCapacity,
		Do: func(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
			// Note: The passed-in context comes from the Prometheus rule group code
			// and does *not* contain the userID. So it needs to be added to the context
			// here before using the context to inject the userID into the HTTP request.
			ctx = user.InjectOrgID(ctx, userID)
			if err := user.InjectOrgIDIntoHTTPRequest(ctx, req); err != nil {
				return nil, err
			}
			return ctxhttp.Do(ctx, client, req)
		},
	}, util.Logger)

	go n.run()

	// This should never fail, unless there's a programming mistake.
	if err := n.applyConfig(r.notifierCfg); err != nil {
		return nil, err
	}

	// TODO: Remove notifiers for stale users. Right now this is a slow leak.
	r.notifiers[userID] = n
	return n.notifier, nil
}

func (r *Ruler) ownsRule(hash uint32) bool {
	rlrs, err := r.ring.Get(hash, ring.Read)
	// If an error occurs evaluate a rule as if it is owned
	// better to have extra datapoints for a rule than none at all
	// TODO: add a temporary cache of owned rule values or something to fall back on
	if err != nil {
		level.Warn(util.Logger).Log("msg", "error reading ring to verify rule group ownership", "err", err)
		ringCheckErrors.Inc()
		return true
	}
	if rlrs.Ingesters[0].Addr == r.lifecycler.Addr {
		level.Debug(util.Logger).Log("msg", "rule group owned", "owner_addr", rlrs.Ingesters[0].Addr, "addr", r.lifecycler.Addr)
		return true
	}
	level.Debug(util.Logger).Log("msg", "rule group not owned, address does not match", "owner_addr", rlrs.Ingesters[0].Addr, "addr", r.lifecycler.Addr)
	return false
}

func (r *Ruler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.cfg.EnableSharding {
		r.ring.ServeHTTP(w, req)
	} else {
		var unshardedPage = `
			<!DOCTYPE html>
			<html>
				<head>
					<meta charset="UTF-8">
					<title>Cortex Ruler Status</title>
				</head>
				<body>
					<h1>Cortex Ruler Status</h1>
					<p>Ruler running with shards disabled</p>
				</body>
			</html>`
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(unshardedPage))
	}
}
