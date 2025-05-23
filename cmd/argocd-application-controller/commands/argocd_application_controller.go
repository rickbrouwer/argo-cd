package commands

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/argoproj/pkg/v2/stats"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	cmdutil "github.com/argoproj/argo-cd/v3/cmd/util"
	commitclient "github.com/argoproj/argo-cd/v3/commitserver/apiclient"
	"github.com/argoproj/argo-cd/v3/common"
	"github.com/argoproj/argo-cd/v3/controller"
	"github.com/argoproj/argo-cd/v3/controller/sharding"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/v3/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/v3/pkg/ratelimiter"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v3/util/argo"
	"github.com/argoproj/argo-cd/v3/util/argo/normalizers"
	cacheutil "github.com/argoproj/argo-cd/v3/util/cache"
	appstatecache "github.com/argoproj/argo-cd/v3/util/cache/appstate"
	"github.com/argoproj/argo-cd/v3/util/cli"
	"github.com/argoproj/argo-cd/v3/util/env"
	"github.com/argoproj/argo-cd/v3/util/errors"
	kubeutil "github.com/argoproj/argo-cd/v3/util/kube"
	"github.com/argoproj/argo-cd/v3/util/settings"
	"github.com/argoproj/argo-cd/v3/util/tls"
	"github.com/argoproj/argo-cd/v3/util/trace"
)

const (
	// CLIName is the name of the CLI
	cliName = common.ApplicationController
	// Default time in seconds for application resync period
	defaultAppResyncPeriod = 120
	// Default time in seconds for application resync period jitter
	defaultAppResyncPeriodJitter = 60
	// Default time in seconds for application hard resync period
	defaultAppHardResyncPeriod = 0
	// Default time in seconds for ignoring consecutive errors when comminicating with repo-server
	defaultRepoErrorGracePeriod = defaultAppResyncPeriod + defaultAppResyncPeriodJitter
)

func NewCommand() *cobra.Command {
	var (
		workqueueRateLimit               ratelimiter.AppControllerRateLimiterConfig
		clientConfig                     clientcmd.ClientConfig
		appResyncPeriod                  int64
		appHardResyncPeriod              int64
		appResyncJitter                  int64
		repoErrorGracePeriod             int64
		repoServerAddress                string
		repoServerTimeoutSeconds         int
		commitServerAddress              string
		selfHealTimeoutSeconds           int
		selfHealBackoffTimeoutSeconds    int
		selfHealBackoffFactor            int
		selfHealBackoffCapSeconds        int
		syncTimeout                      int
		statusProcessors                 int
		operationProcessors              int
		glogLevel                        int
		metricsPort                      int
		metricsCacheExpiration           time.Duration
		metricsAplicationLabels          []string
		metricsAplicationConditions      []string
		metricsClusterLabels             []string
		kubectlParallelismLimit          int64
		cacheSource                      func() (*appstatecache.Cache, error)
		redisClient                      *redis.Client
		repoServerPlaintext              bool
		repoServerStrictTLS              bool
		otlpAddress                      string
		otlpInsecure                     bool
		otlpHeaders                      map[string]string
		otlpAttrs                        []string
		applicationNamespaces            []string
		persistResourceHealth            bool
		shardingAlgorithm                string
		enableDynamicClusterDistribution bool
		serverSideDiff                   bool
		ignoreNormalizerOpts             normalizers.IgnoreNormalizerOpts

		// argocd k8s event logging flag
		enableK8sEvent  []string
		hydratorEnabled bool
	)
	command := cobra.Command{
		Use:               cliName,
		Short:             "Run ArgoCD Application Controller",
		Long:              "ArgoCD application controller is a Kubernetes controller that continuously monitors running applications and compares the current, live state against the desired target state (as specified in the repo). This command runs Application Controller in the foreground.  It can be configured by following options.",
		DisableAutoGenTag: true,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx, cancel := context.WithCancel(c.Context())
			defer cancel()

			vers := common.GetVersion()
			namespace, _, err := clientConfig.Namespace()
			errors.CheckError(err)
			vers.LogStartupInfo(
				"ArgoCD Application Controller",
				map[string]any{
					"namespace": namespace,
				},
			)

			cli.SetLogFormat(cmdutil.LogFormat)
			cli.SetLogLevel(cmdutil.LogLevel)
			cli.SetGLogLevel(glogLevel)

			// Recover from panic and log the error using the configured logger instead of the default.
			defer func() {
				if r := recover(); r != nil {
					log.WithField("trace", string(debug.Stack())).Fatal("Recovered from panic: ", r)
				}
			}()

			config, err := clientConfig.ClientConfig()
			errors.CheckError(err)
			errors.CheckError(v1alpha1.SetK8SConfigDefaults(config))
			config.UserAgent = fmt.Sprintf("%s/%s (%s)", common.DefaultApplicationControllerName, vers.Version, vers.Platform)

			kubeClient := kubernetes.NewForConfigOrDie(config)
			appClient := appclientset.NewForConfigOrDie(config)

			hardResyncDuration := time.Duration(appHardResyncPeriod) * time.Second

			var resyncDuration time.Duration
			if appResyncPeriod == 0 {
				// Re-sync should be disabled if period is 0. Set duration to a very long duration
				resyncDuration = time.Hour * 24 * 365 * 100
			} else {
				resyncDuration = time.Duration(appResyncPeriod) * time.Second
			}

			tlsConfig := apiclient.TLSConfiguration{
				DisableTLS:       repoServerPlaintext,
				StrictValidation: repoServerStrictTLS,
			}

			// Load CA information to use for validating connections to the
			// repository server, if strict TLS validation was requested.
			if !repoServerPlaintext && repoServerStrictTLS {
				pool, err := tls.LoadX509CertPool(
					env.StringFromEnv(common.EnvAppConfigPath, common.DefaultAppConfigPath)+"/controller/tls/tls.crt",
					env.StringFromEnv(common.EnvAppConfigPath, common.DefaultAppConfigPath)+"/controller/tls/ca.crt",
				)
				if err != nil {
					log.Fatalf("%v", err)
				}
				tlsConfig.Certificates = pool
			}

			repoClientset := apiclient.NewRepoServerClientset(repoServerAddress, repoServerTimeoutSeconds, tlsConfig)

			commitClientset := commitclient.NewCommitServerClientset(commitServerAddress)

			cache, err := cacheSource()
			errors.CheckError(err)
			cache.Cache.SetClient(cacheutil.NewTwoLevelClient(cache.Cache.GetClient(), 10*time.Minute))

			var appController *controller.ApplicationController

			settingsMgr := settings.NewSettingsManager(ctx, kubeClient, namespace, settings.WithRepoOrClusterChangedHandler(func() {
				appController.InvalidateProjectsCache()
			}))
			kubectl := kubeutil.NewKubectl()
			clusterSharding, err := sharding.GetClusterSharding(kubeClient, settingsMgr, shardingAlgorithm, enableDynamicClusterDistribution)
			errors.CheckError(err)
			var selfHealBackoff *wait.Backoff
			if selfHealBackoffTimeoutSeconds != 0 {
				selfHealBackoff = &wait.Backoff{
					Duration: time.Duration(selfHealBackoffTimeoutSeconds) * time.Second,
					Factor:   float64(selfHealBackoffFactor),
					Cap:      time.Duration(selfHealBackoffCapSeconds) * time.Second,
				}
			}
			appController, err = controller.NewApplicationController(
				namespace,
				settingsMgr,
				kubeClient,
				appClient,
				repoClientset,
				commitClientset,
				cache,
				kubectl,
				resyncDuration,
				hardResyncDuration,
				time.Duration(appResyncJitter)*time.Second,
				time.Duration(selfHealTimeoutSeconds)*time.Second,
				selfHealBackoff,
				time.Duration(syncTimeout)*time.Second,
				time.Duration(repoErrorGracePeriod)*time.Second,
				metricsPort,
				metricsCacheExpiration,
				metricsAplicationLabels,
				metricsAplicationConditions,
				metricsClusterLabels,
				kubectlParallelismLimit,
				persistResourceHealth,
				clusterSharding,
				applicationNamespaces,
				&workqueueRateLimit,
				serverSideDiff,
				enableDynamicClusterDistribution,
				ignoreNormalizerOpts,
				enableK8sEvent,
				hydratorEnabled,
			)
			errors.CheckError(err)
			cacheutil.CollectMetrics(redisClient, appController.GetMetricsServer(), nil)

			stats.RegisterStackDumper()
			stats.StartStatsTicker(10 * time.Minute)
			stats.RegisterHeapDumper("memprofile")

			if otlpAddress != "" {
				closeTracer, err := trace.InitTracer(ctx, "argocd-controller", otlpAddress, otlpInsecure, otlpHeaders, otlpAttrs)
				if err != nil {
					log.Fatalf("failed to initialize tracing: %v", err)
				}
				defer closeTracer()
			}

			// Graceful shutdown code
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			go func() {
				s := <-sigCh
				log.Printf("got signal %v, attempting graceful shutdown", s)
				cancel()
			}()

			go appController.Run(ctx, statusProcessors, operationProcessors)

			<-ctx.Done()

			log.Println("clean shutdown")

			return nil
		},
	}

	clientConfig = cli.AddKubectlFlagsToCmd(&command)
	command.Flags().Int64Var(&appResyncPeriod, "app-resync", int64(env.ParseDurationFromEnv("ARGOCD_RECONCILIATION_TIMEOUT", defaultAppResyncPeriod*time.Second, 0, math.MaxInt64).Seconds()), "Time period in seconds for application resync.")
	command.Flags().Int64Var(&appHardResyncPeriod, "app-hard-resync", int64(env.ParseDurationFromEnv("ARGOCD_HARD_RECONCILIATION_TIMEOUT", defaultAppHardResyncPeriod*time.Second, 0, math.MaxInt64).Seconds()), "Time period in seconds for application hard resync.")
	command.Flags().Int64Var(&appResyncJitter, "app-resync-jitter", int64(env.ParseDurationFromEnv("ARGOCD_RECONCILIATION_JITTER", defaultAppResyncPeriodJitter*time.Second, 0, math.MaxInt64).Seconds()), "Maximum time period in seconds to add as a delay jitter for application resync.")
	command.Flags().Int64Var(&repoErrorGracePeriod, "repo-error-grace-period-seconds", int64(env.ParseDurationFromEnv("ARGOCD_REPO_ERROR_GRACE_PERIOD_SECONDS", defaultRepoErrorGracePeriod*time.Second, 0, math.MaxInt64).Seconds()), "Grace period in seconds for ignoring consecutive errors while communicating with repo server.")
	command.Flags().StringVar(&repoServerAddress, "repo-server", env.StringFromEnv("ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER", common.DefaultRepoServerAddr), "Repo server address.")
	command.Flags().IntVar(&repoServerTimeoutSeconds, "repo-server-timeout-seconds", env.ParseNumFromEnv("ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER_TIMEOUT_SECONDS", 60, 0, math.MaxInt64), "Repo server RPC call timeout seconds.")
	command.Flags().StringVar(&commitServerAddress, "commit-server", env.StringFromEnv("ARGOCD_APPLICATION_CONTROLLER_COMMIT_SERVER", common.DefaultCommitServerAddr), "Commit server address.")
	command.Flags().IntVar(&statusProcessors, "status-processors", env.ParseNumFromEnv("ARGOCD_APPLICATION_CONTROLLER_STATUS_PROCESSORS", 20, 0, math.MaxInt32), "Number of application status processors")
	command.Flags().IntVar(&operationProcessors, "operation-processors", env.ParseNumFromEnv("ARGOCD_APPLICATION_CONTROLLER_OPERATION_PROCESSORS", 10, 0, math.MaxInt32), "Number of application operation processors")
	command.Flags().StringVar(&cmdutil.LogFormat, "logformat", env.StringFromEnv("ARGOCD_APPLICATION_CONTROLLER_LOGFORMAT", "json"), "Set the logging format. One of: json|text")
	command.Flags().StringVar(&cmdutil.LogLevel, "loglevel", env.StringFromEnv("ARGOCD_APPLICATION_CONTROLLER_LOGLEVEL", "info"), "Set the logging level. One of: debug|info|warn|error")
	command.Flags().IntVar(&glogLevel, "gloglevel", 0, "Set the glog logging level")
	command.Flags().IntVar(&metricsPort, "metrics-port", common.DefaultPortArgoCDMetrics, "Start metrics server on given port")
	command.Flags().DurationVar(&metricsCacheExpiration, "metrics-cache-expiration", env.ParseDurationFromEnv("ARGOCD_APPLICATION_CONTROLLER_METRICS_CACHE_EXPIRATION", 0*time.Second, 0, math.MaxInt64), "Prometheus metrics cache expiration (disabled  by default. e.g. 24h0m0s)")
	command.Flags().IntVar(&selfHealTimeoutSeconds, "self-heal-timeout-seconds", env.ParseNumFromEnv("ARGOCD_APPLICATION_CONTROLLER_SELF_HEAL_TIMEOUT_SECONDS", 0, 0, math.MaxInt32), "Specifies timeout between application self heal attempts")
	command.Flags().IntVar(&selfHealBackoffTimeoutSeconds, "self-heal-backoff-timeout-seconds", env.ParseNumFromEnv("ARGOCD_APPLICATION_CONTROLLER_SELF_HEAL_BACKOFF_TIMEOUT_SECONDS", 2, 0, math.MaxInt32), "Specifies initial timeout of exponential backoff between self heal attempts")
	command.Flags().IntVar(&selfHealBackoffFactor, "self-heal-backoff-factor", env.ParseNumFromEnv("ARGOCD_APPLICATION_CONTROLLER_SELF_HEAL_BACKOFF_FACTOR", 3, 0, math.MaxInt32), "Specifies factor of exponential timeout between application self heal attempts")
	command.Flags().IntVar(&selfHealBackoffCapSeconds, "self-heal-backoff-cap-seconds", env.ParseNumFromEnv("ARGOCD_APPLICATION_CONTROLLER_SELF_HEAL_BACKOFF_CAP_SECONDS", 300, 0, math.MaxInt32), "Specifies max timeout of exponential backoff between application self heal attempts")
	command.Flags().IntVar(&syncTimeout, "sync-timeout", env.ParseNumFromEnv("ARGOCD_APPLICATION_CONTROLLER_SYNC_TIMEOUT", 0, 0, math.MaxInt32), "Specifies the timeout after which a sync would be terminated. 0 means no timeout (default 0).")
	command.Flags().Int64Var(&kubectlParallelismLimit, "kubectl-parallelism-limit", env.ParseInt64FromEnv("ARGOCD_APPLICATION_CONTROLLER_KUBECTL_PARALLELISM_LIMIT", 20, 0, math.MaxInt64), "Number of allowed concurrent kubectl fork/execs. Any value less than 1 means no limit.")
	command.Flags().BoolVar(&repoServerPlaintext, "repo-server-plaintext", env.ParseBoolFromEnv("ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER_PLAINTEXT", false), "Disable TLS on connections to repo server")
	command.Flags().BoolVar(&repoServerStrictTLS, "repo-server-strict-tls", env.ParseBoolFromEnv("ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER_STRICT_TLS", false), "Whether to use strict validation of the TLS cert presented by the repo server")
	command.Flags().StringSliceVar(&metricsAplicationLabels, "metrics-application-labels", []string{}, "List of Application labels that will be added to the argocd_application_labels metric")
	command.Flags().StringSliceVar(&metricsAplicationConditions, "metrics-application-conditions", []string{}, "List of Application conditions that will be added to the argocd_application_conditions metric")
	command.Flags().StringSliceVar(&metricsClusterLabels, "metrics-cluster-labels", []string{}, "List of Cluster labels that will be added to the argocd_cluster_labels metric")
	command.Flags().StringVar(&otlpAddress, "otlp-address", env.StringFromEnv("ARGOCD_APPLICATION_CONTROLLER_OTLP_ADDRESS", ""), "OpenTelemetry collector address to send traces to")
	command.Flags().BoolVar(&otlpInsecure, "otlp-insecure", env.ParseBoolFromEnv("ARGOCD_APPLICATION_CONTROLLER_OTLP_INSECURE", true), "OpenTelemetry collector insecure mode")
	command.Flags().StringToStringVar(&otlpHeaders, "otlp-headers", env.ParseStringToStringFromEnv("ARGOCD_APPLICATION_CONTROLLER_OTLP_HEADERS", map[string]string{}, ","), "List of OpenTelemetry collector extra headers sent with traces, headers are comma-separated key-value pairs(e.g. key1=value1,key2=value2)")
	command.Flags().StringSliceVar(&otlpAttrs, "otlp-attrs", env.StringsFromEnv("ARGOCD_APPLICATION_CONTROLLER_OTLP_ATTRS", []string{}, ","), "List of OpenTelemetry collector extra attrs when send traces, each attribute is separated by a colon(e.g. key:value)")
	command.Flags().StringSliceVar(&applicationNamespaces, "application-namespaces", env.StringsFromEnv("ARGOCD_APPLICATION_NAMESPACES", []string{}, ","), "List of additional namespaces that applications are allowed to be reconciled from")
	command.Flags().BoolVar(&persistResourceHealth, "persist-resource-health", env.ParseBoolFromEnv("ARGOCD_APPLICATION_CONTROLLER_PERSIST_RESOURCE_HEALTH", false), "Enables storing the managed resources health in the Application CRD")
	command.Flags().StringVar(&shardingAlgorithm, "sharding-method", env.StringFromEnv(common.EnvControllerShardingAlgorithm, common.DefaultShardingAlgorithm), "Enables choice of sharding method. Supported sharding methods are : [legacy, round-robin, consistent-hashing] ")
	// global queue rate limit config
	command.Flags().Int64Var(&workqueueRateLimit.BucketSize, "wq-bucket-size", env.ParseInt64FromEnv("WORKQUEUE_BUCKET_SIZE", 500, 1, math.MaxInt64), "Set Workqueue Rate Limiter Bucket Size, default 500")
	command.Flags().Float64Var(&workqueueRateLimit.BucketQPS, "wq-bucket-qps", env.ParseFloat64FromEnv("WORKQUEUE_BUCKET_QPS", math.MaxFloat64, 1, math.MaxFloat64), "Set Workqueue Rate Limiter Bucket QPS, default set to MaxFloat64 which disables the bucket limiter")
	// individual item rate limit config
	// when WORKQUEUE_FAILURE_COOLDOWN is 0 per item rate limiting is disabled(default)
	command.Flags().DurationVar(&workqueueRateLimit.FailureCoolDown, "wq-cooldown-ns", time.Duration(env.ParseInt64FromEnv("WORKQUEUE_FAILURE_COOLDOWN_NS", 0, 0, (24*time.Hour).Nanoseconds())), "Set Workqueue Per Item Rate Limiter Cooldown duration in ns, default 0(per item rate limiter disabled)")
	command.Flags().DurationVar(&workqueueRateLimit.BaseDelay, "wq-basedelay-ns", time.Duration(env.ParseInt64FromEnv("WORKQUEUE_BASE_DELAY_NS", time.Millisecond.Nanoseconds(), time.Nanosecond.Nanoseconds(), (24*time.Hour).Nanoseconds())), "Set Workqueue Per Item Rate Limiter Base Delay duration in nanoseconds, default 1000000 (1ms)")
	command.Flags().DurationVar(&workqueueRateLimit.MaxDelay, "wq-maxdelay-ns", time.Duration(env.ParseInt64FromEnv("WORKQUEUE_MAX_DELAY_NS", time.Second.Nanoseconds(), 1*time.Millisecond.Nanoseconds(), (24*time.Hour).Nanoseconds())), "Set Workqueue Per Item Rate Limiter Max Delay duration in nanoseconds, default 1000000000 (1s)")
	command.Flags().Float64Var(&workqueueRateLimit.BackoffFactor, "wq-backoff-factor", env.ParseFloat64FromEnv("WORKQUEUE_BACKOFF_FACTOR", 1.5, 0, math.MaxFloat64), "Set Workqueue Per Item Rate Limiter Backoff Factor, default is 1.5")
	command.Flags().BoolVar(&enableDynamicClusterDistribution, "dynamic-cluster-distribution-enabled", env.ParseBoolFromEnv(common.EnvEnableDynamicClusterDistribution, false), "Enables dynamic cluster distribution.")
	command.Flags().BoolVar(&serverSideDiff, "server-side-diff-enabled", env.ParseBoolFromEnv(common.EnvServerSideDiff, false), "Feature flag to enable ServerSide diff. Default (\"false\")")
	command.Flags().DurationVar(&ignoreNormalizerOpts.JQExecutionTimeout, "ignore-normalizer-jq-execution-timeout-seconds", env.ParseDurationFromEnv("ARGOCD_IGNORE_NORMALIZER_JQ_TIMEOUT", 0*time.Second, 0, math.MaxInt64), "Set ignore normalizer JQ execution timeout")
	// argocd k8s event logging flag
	command.Flags().StringSliceVar(&enableK8sEvent, "enable-k8s-event", env.StringsFromEnv("ARGOCD_ENABLE_K8S_EVENT", argo.DefaultEnableEventList(), ","), "Enable ArgoCD to use k8s event. For disabling all events, set the value as `none`. (e.g --enable-k8s-event=none), For enabling specific events, set the value as `event reason`. (e.g --enable-k8s-event=StatusRefreshed,ResourceCreated)")
	command.Flags().BoolVar(&hydratorEnabled, "hydrator-enabled", env.ParseBoolFromEnv("ARGOCD_HYDRATOR_ENABLED", false), "Feature flag to enable Hydrator. Default (\"false\")")
	cacheSource = appstatecache.AddCacheFlagsToCmd(&command, cacheutil.Options{
		OnClientCreated: func(client *redis.Client) {
			redisClient = client
		},
	})
	return &command
}
