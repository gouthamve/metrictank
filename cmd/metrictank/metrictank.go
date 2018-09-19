package main

import (
	"flag"
	"fmt"
	l "log"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "net/http/pprof"

	"github.com/Dieterbe/profiletrigger/heap"
	"github.com/Shopify/sarama"
	"github.com/grafana/metrictank/api"
	"github.com/grafana/metrictank/cluster"
	"github.com/grafana/metrictank/conf"
	"github.com/grafana/metrictank/idx"
	"github.com/grafana/metrictank/idx/cassandra"
	"github.com/grafana/metrictank/idx/memory"
	"github.com/grafana/metrictank/input"
	inCarbon "github.com/grafana/metrictank/input/carbon"
	inKafkaMdm "github.com/grafana/metrictank/input/kafkamdm"
	inPrometheus "github.com/grafana/metrictank/input/prometheus"
	"github.com/grafana/metrictank/mdata"
	"github.com/grafana/metrictank/mdata/cache"
	"github.com/grafana/metrictank/mdata/notifierKafka"
	"github.com/grafana/metrictank/mdata/notifierNsq"
	"github.com/grafana/metrictank/stats"
	statsConfig "github.com/grafana/metrictank/stats/config"
	cassandraStore "github.com/grafana/metrictank/store/cassandra"
	"github.com/raintank/dur"
	"github.com/raintank/worldping-api/pkg/log"
	"github.com/rakyll/globalconf"
)

var (
	logLevel     int
	warmupPeriod time.Duration
	startupTime  time.Time
	gitHash      = "(none)"

	metrics     *mdata.AggMetrics
	metricIndex idx.MetricIndex
	apiServer   *api.Server
	inputs      []input.Plugin
	store       mdata.Store

	// Misc:
	instance    = flag.String("instance", "default", "instance identifier. must be unique. used in clustering messages, for naming queue consumers and emitted metrics")
	showVersion = flag.Bool("version", false, "print version string")
	confFile    = flag.String("config", "/etc/metrictank/metrictank.ini", "configuration file path")

	// Data:
	dropFirstChunk    = flag.Bool("drop-first-chunk", false, "forego persisting of first received (and typically incomplete) chunk")
	chunkMaxStaleStr  = flag.String("chunk-max-stale", "1h", "max age for a chunk before to be considered stale and to be persisted to Cassandra.")
	metricMaxStaleStr = flag.String("metric-max-stale", "6h", "max age for a metric before to be considered stale and to be purged from memory.")
	gcIntervalStr     = flag.String("gc-interval", "1h", "Interval to run garbage collection job.")
	warmUpPeriodStr   = flag.String("warm-up-period", "1h", "duration before secondary nodes start serving requests")
	publicOrg         = flag.Int("public-org", 0, "org Id for publically (any org) accessible data. leave 0 to disable")

	// Profiling, instrumentation and logging:
	blockProfileRate = flag.Int("block-profile-rate", 0, "see https://golang.org/pkg/runtime/#SetBlockProfileRate")
	memProfileRate   = flag.Int("mem-profile-rate", 512*1024, "0 to disable. 1 for max precision (expensive!) see https://golang.org/pkg/runtime/#pkg-variables")

	proftrigPath       = flag.String("proftrigger-path", "/tmp", "path to store triggered profiles")
	proftrigFreqStr    = flag.String("proftrigger-freq", "60s", "inspect status frequency. set to 0 to disable")
	proftrigMinDiffStr = flag.String("proftrigger-min-diff", "1h", "minimum time between triggered profiles")
	proftrigHeapThresh = flag.Int("proftrigger-heap-thresh", 25000000000, "if this many bytes allocated, trigger a profile")
)

func init() {
	flag.IntVar(&logLevel, "log-level", 2, "log level. 0=TRACE|1=DEBUG|2=INFO|3=WARN|4=ERROR|5=CRITICAL|6=FATAL")
}

func main() {
	startupTime = time.Now()

	flag.Parse()

	// if the user just wants the version, give it and exit
	if *showVersion {
		fmt.Printf("metrictank (built with %s, git hash %s)\n", runtime.Version(), gitHash)
		return
	}

	// Only try and parse the conf file if it exists
	path := ""
	if _, err := os.Stat(*confFile); err == nil {
		path = *confFile
	}
	config, err := globalconf.NewWithOptions(&globalconf.Options{
		Filename:  path,
		EnvPrefix: "MT_",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: configuration file error: %s", err)
		os.Exit(1)
	}
	// load config for metric ingestors
	inCarbon.ConfigSetup()
	inKafkaMdm.ConfigSetup()
	inPrometheus.ConfigSetup()

	// load config for cluster handlers
	notifierNsq.ConfigSetup()

	// load config for metricIndexers
	memory.ConfigSetup()
	cassandra.ConfigSetup()

	// load config for API
	api.ConfigSetup()

	// load config for cluster
	cluster.ConfigSetup()

	// stats
	statsConfig.ConfigSetup()

	// storage-schemas, storage-aggregation files
	mdata.ConfigSetup()

	// cassandra Store
	cassandraStore.ConfigSetup()

	config.ParseAll()

	/***********************************
		Set up Logger
	***********************************/

	log.NewLogger(0, "console", fmt.Sprintf(`{"level": %d, "formatting":false}`, logLevel))

	mdata.LogLevel = logLevel
	memory.LogLevel = logLevel
	inKafkaMdm.LogLevel = logLevel
	api.LogLevel = logLevel
	// workaround for https://github.com/grafana/grafana/issues/4055
	switch logLevel {
	case 0:
		log.Level(log.TRACE)
	case 1:
		log.Level(log.DEBUG)
	case 2:
		log.Level(log.INFO)
	case 3:
		log.Level(log.WARN)
	case 4:
		log.Level(log.ERROR)
	case 5:
		log.Level(log.CRITICAL)
	case 6:
		log.Level(log.FATAL)
	}

	/***********************************
		Validate  settings needed for clustering
	***********************************/
	if *instance == "" {
		log.Fatal(4, "instance can't be empty")
	}

	/***********************************
		Initialize our Cluster
	***********************************/
	api.ConfigProcess()
	cluster.ConfigProcess()
	scheme := "http"
	if api.UseSSL {
		scheme = "https"
	}
	addrParts := strings.Split(api.Addr, ":")
	port, err := strconv.ParseInt(addrParts[len(addrParts)-1], 10, 64)
	if err != nil {
		log.Fatal(4, "Could not parse port from listenAddr. %s", api.Addr)
	}
	cluster.Init(*instance, gitHash, startupTime, scheme, int(port))

	/***********************************
		Validate remaining settings
	***********************************/
	inCarbon.ConfigProcess()
	inKafkaMdm.ConfigProcess(*instance)
	inPrometheus.ConfigProcess()
	notifierNsq.ConfigProcess()
	notifierKafka.ConfigProcess(*instance)
	statsConfig.ConfigProcess(*instance)
	mdata.ConfigProcess()

	if !inCarbon.Enabled && !inKafkaMdm.Enabled && !inPrometheus.Enabled {
		log.Fatal(4, "you should enable at least 1 input plugin")
	}

	sec := dur.MustParseNDuration("warm-up-period", *warmUpPeriodStr)
	warmupPeriod = time.Duration(sec) * time.Second

	chunkMaxStale := dur.MustParseNDuration("chunk-max-stale", *chunkMaxStaleStr)
	metricMaxStale := dur.MustParseNDuration("metric-max-stale", *metricMaxStaleStr)
	gcInterval := time.Duration(dur.MustParseNDuration("gc-interval", *gcIntervalStr)) * time.Second

	proftrigFreq := dur.MustParseDuration("proftrigger-freq", *proftrigFreqStr)
	proftrigMinDiff := int(dur.MustParseNDuration("proftrigger-min-diff", *proftrigMinDiffStr))
	if proftrigFreq > 0 {
		errors := make(chan error)
		trigger, _ := heap.New(*proftrigPath, *proftrigHeapThresh, proftrigMinDiff, time.Duration(proftrigFreq)*time.Second, errors)
		go func() {
			for e := range errors {
				log.Error(0, "profiletrigger heap: %s", e)
			}
		}()
		go trigger.Run()
	}

	/***********************************
		configure Profiling
	***********************************/
	runtime.SetBlockProfileRate(*blockProfileRate)
	runtime.MemProfileRate = *memProfileRate

	/************************************
	    handle interrupt signals
	************************************/
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	/***********************************
		Report Version
	***********************************/
	log.Info("Metrictank starting. Built from %s - Go version %s", gitHash, runtime.Version())
	// metric version.%s is the version of metrictank running.  The metric value is always 1
	mtVersion := stats.NewBool(fmt.Sprintf("version.%s", strings.Replace(gitHash, ".", "_", -1)))
	mtVersion.Set(true)

	/***********************************
		collect stats
	***********************************/
	statsConfig.Start()

	/***********************************
		Initialize tracer
	***********************************/
	tracer, traceCloser, err := conf.GetTracer()
	if err != nil {
		log.Fatal(4, "Could not initialize jaeger tracer: %s", err.Error())
	}
	defer traceCloser.Close()

	/***********************************
		Initialize our backendStore
	***********************************/
	store, err = cassandraStore.NewCassandraStore(cassandraStore.CliConfig, mdata.TTLs())
	if err != nil {
		log.Fatal(4, "failed to initialize cassandra. %s", err)
	}
	store.SetTracer(tracer)

	/***********************************
		Initialize the Chunk Cache
	***********************************/
	ccache := cache.NewCCache()
	ccache.SetTracer(tracer)

	/***********************************
		Initialize our MemoryStore
	***********************************/
	metrics = mdata.NewAggMetrics(store, ccache, *dropFirstChunk, chunkMaxStale, metricMaxStale, gcInterval)

	/***********************************
		Initialize our Inputs
	***********************************/
	// note. all these New functions must either return a valid instance or call log.Fatal
	if inCarbon.Enabled {
		inputs = append(inputs, inCarbon.New())
	}

	if inPrometheus.Enabled {
		inputs = append(inputs, inPrometheus.New())
	}

	if inKafkaMdm.Enabled {
		sarama.Logger = l.New(os.Stdout, "[Sarama] ", l.LstdFlags)
		inputs = append(inputs, inKafkaMdm.New())
	}

	if cluster.Mode == cluster.ModeMulti && len(inputs) > 1 {
		log.Warn("It is not recommended to run a multi-node cluster with more than 1 input plugin.")
	}

	/***********************************
	    Start the ClusterManager
	***********************************/
	cluster.Start()

	/***********************************
		Initialize our MetricIdx
	***********************************/
	pre := time.Now()

	if *publicOrg < 0 {
		log.Fatal(4, "public-org cannot be <0")
	}

	idx.OrgIdPublic = uint32(*publicOrg)

	if memory.Enabled {
		if metricIndex != nil {
			log.Fatal(4, "Only 1 metricIndex handler can be enabled.")
		}
		metricIndex = memory.New()
	}
	if cassandra.Enabled {
		if metricIndex != nil {
			log.Fatal(4, "Only 1 metricIndex handler can be enabled.")
		}
		metricIndex = cassandra.New()
	}

	if metricIndex == nil {
		log.Fatal(4, "No metricIndex handlers enabled.")
	}

	/***********************************
		Initialize our API server
	***********************************/
	apiServer, err = api.NewServer()
	if err != nil {
		log.Fatal(4, "Failed to start API. %s", err.Error())
	}

	apiServer.BindMetricIndex(metricIndex)
	apiServer.BindMemoryStore(metrics)
	apiServer.BindBackendStore(store)
	apiServer.BindCache(ccache)
	apiServer.BindTracer(tracer)
	apiServer.BindPromQueryEngine()
	cluster.Tracer = tracer
	go apiServer.Run()

	/***********************************
		Load index entries from the backend store.
	***********************************/
	err = metricIndex.Init()
	if err != nil {
		log.Fatal(4, "failed to initialize metricIndex: %s", err)
	}
	log.Info("metricIndex initialized in %s. starting data consumption", time.Now().Sub(pre))

	/***********************************
		Initialize MetricPersist notifiers
	***********************************/
	handlers := make([]mdata.NotifierHandler, 0)
	if notifierKafka.Enabled {
		// The notifierKafka handler will block here until it has processed the backlog of metricPersist messages.
		// it will block for at most kafka-cluster.backlog-process-timeout (default 60s)
		handlers = append(handlers, notifierKafka.New(*instance, metrics, metricIndex))
	}

	if notifierNsq.Enabled {
		handlers = append(handlers, notifierNsq.New(*instance, metrics, metricIndex))
	}

	mdata.InitPersistNotifier(handlers...)

	/***********************************
		Start our inputs
	***********************************/
	pluginFatal := make(chan struct{})
	for _, plugin := range inputs {
		if carbonPlugin, ok := plugin.(*inCarbon.Carbon); ok {
			carbonPlugin.IntervalGetter(inCarbon.NewIndexIntervalGetter(metricIndex))
		}
		err = plugin.Start(input.NewDefaultHandler(metrics, metricIndex, plugin.Name()), pluginFatal)
		if err != nil {
			shutdown()
			return
		}
		plugin.MaintainPriority()
		apiServer.BindPrioritySetter(plugin)
	}

	// metric cluster.self.promotion_wait is how long a candidate (secondary node) has to wait until it can become a primary
	// When the timer becomes 0 it means the in-memory buffer has been able to fully populate so that if you stop a primary
	// and it was able to save its complete chunks, this node will be able to take over without dataloss.
	// You can upgrade a candidate to primary while the timer is not 0 yet, it just means it may have missing data in the chunks that it will save.
	maxChunkSpan := mdata.MaxChunkSpan()
	stats.NewTimeDiffReporter32("cluster.self.promotion_wait", (uint32(time.Now().Unix())/maxChunkSpan+1)*maxChunkSpan)

	/***********************************
		Set our status so we can accept
		requests from users.
	***********************************/
	if cluster.Manager.IsPrimary() {
		cluster.Manager.SetReady()
	} else {
		time.AfterFunc(warmupPeriod, cluster.Manager.SetReady)
	}

	/***********************************
		Wait for Shutdown
	***********************************/
	select {
	case sig := <-sigChan:
		log.Info("Received signal %q. Shutting down", sig)
	case <-pluginFatal:
		log.Info("An input plugin signalled a fatal error. Shutting down")
	}
	shutdown()
}

func shutdown() {
	// Leave the cluster. All other nodes will be notified we have left
	// and so will stop sending us requests.
	cluster.Stop()

	// stop API
	apiServer.Stop()

	// shutdown our input plugins.  These may take a while as we allow them
	// to finish processing any metrics that have already been ingested.
	timer := time.NewTimer(time.Second * 10)
	var wg sync.WaitGroup
	for _, plugin := range inputs {
		wg.Add(1)
		go func(plugin input.Plugin) {
			log.Info("Shutting down %s consumer", plugin.Name())
			plugin.Stop()
			log.Info("%s consumer finished shutdown", plugin.Name())
			wg.Done()
		}(plugin)
	}
	pluginsStopped := make(chan struct{})
	go func() {
		wg.Wait()
		close(pluginsStopped)
	}()
	select {
	case <-timer.C:
		log.Warn("Plugins taking too long to shutdown, not waiting any longer.")
	case <-pluginsStopped:
		timer.Stop()
	}

	log.Info("closing store")
	store.Stop()
	metricIndex.Stop()
	log.Info("terminating.")
	log.Close()
}
