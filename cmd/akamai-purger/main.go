package notmain

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/honeycombio/beeline-go"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/letsencrypt/boulder/akamai"
	akamaipb "github.com/letsencrypt/boulder/akamai/proto"
	"github.com/letsencrypt/boulder/cmd"
	bgrpc "github.com/letsencrypt/boulder/grpc"
	blog "github.com/letsencrypt/boulder/log"
)

const (
	// TODO(#6003) remove entirely.
	DeprecatedResponsesPerBatch = 33

	// akamaiBytesPerResponse is the total bytes of all 3 URLs associated with a
	// single OCSP response cached by Akamai. Each response is composed of 3
	// URLs; the POST Cache Key URL is 61 bytes and the encoded and unencoded
	// GET URLs are 163 bytes and 151 bytes respectively. This totals 375 bytes,
	// which we round up to 400.
	akamaiBytesPerResponse = 400

	// defaultResponsesPerBatch is the number of responses sent in each request
	// to the Akamai Fast-Purge API when the configuration key
	// 'responsesPerBatch' is left unspecified in the 'throughput' mapping of
	// the configuration file.
	defaultResponsesPerBatch = 2

	// defaultResponsesPerBatch duration waited between requests sent to the
	// Akamai Fast-Purge API when the configuration key 'purgBatchEvery' is left
	// unspecified in the 'throughput' mapping of the configuration file.
	defaultPurgeBatchEvery = time.Millisecond * 32

	// defaultQueueSize is the size of the queue when the configuration key
	// 'maxQueueSize' is unspecified. A queue size of 1.25M cached OCSP
	// responses, assuming 3 URLs per request, is about 6 hours of work using
	// the default settings detailed above.
	defaultQueueSize = 1250000

	// akamaiBytesPerReqLimit is the limit of bytes allowed in a single request
	// to the Fast-Purge API. With a limit of no more than 50,000 bytes, we
	// subtract 1 byte to get the limit, and subtract an additional 19 bytes for
	// overhead of the 'objects' key and array.
	akamaiBytesPerReqLimit = 50000 - 1 - 19

	// akamaiAPIReqPerSecondLimit is the limit of requests, per second, that
	// we're allowed to make to the Fast-Purge API.
	akamaiAPIReqPerSecondLimit = 50

	// akamaiURLsPerSecondLimit is the limit of URLs, sent per second, that
	// we're allowed to make to the Fast-Purge API.
	akamaiURLsPerSecondLimit = 200
)

// Throughput is a container for all throuput related akamai-purger
// configuration settings.
type Throughput struct {

	// ResponsesPerBatch the number of cached OCSP responses to included in each
	// purge request. One cached OCSP response is composed of 3 URLs totaling <
	// 400 bytes. If this value isn't provided it will default to
	// 'defaultResponsesPerBatch'.
	ResponsesPerBatch int

	// PurgeBatchEvery is the duration waited between dispatching an Akamai
	// purge request containing 'ResponsesPerBatch' * 3 URLs. If this value
	// isn't provided it will default to 'defaultPurgeBatchEvery'.
	PurgeBatchEvery cmd.ConfigDuration
}

func (t *Throughput) useOptimizedDefaults() {
	if t.ResponsesPerBatch == 0 {
		t.ResponsesPerBatch = defaultResponsesPerBatch
	}
	if t.PurgeBatchEvery.Duration == 0 {
		t.PurgeBatchEvery.Duration = defaultPurgeBatchEvery
	}
}

// validate ensures that the provided throughput configuration will not violate
// the Akamai Fast-Purge API limits. For more information see the official
// documentation:
// https://techdocs.akamai.com/purge-cache/reference/rate-limiting
func (t *Throughput) validate() error {
	if t.PurgeBatchEvery.Duration == 0 {
		// TODO(#6003) remove /'purgeInterval'.
		return errors.New("'purgeBatchEvery'/'purgeInterval' must be > 0 nanoseconds")
	}
	if t.ResponsesPerBatch <= 0 {
		return errors.New("'responsesPerBatch' must be > 0")
	}

	// Send no more than the 50,000 bytes of objects we’re allotted per request.
	bytesPerRequest := (t.ResponsesPerBatch * akamaiBytesPerResponse)
	if bytesPerRequest > akamaiBytesPerReqLimit {
		return fmt.Errorf("config exceeds Akamai's bytes per request limit (%d bytes) by %d",
			akamaiBytesPerReqLimit, bytesPerRequest-akamaiBytesPerReqLimit)
	}

	// Send no more than the 50 API requests we’re allotted each second.
	requestsPerSecond := int(math.Ceil(float64(time.Second) / float64(t.PurgeBatchEvery.Duration)))
	if requestsPerSecond > akamaiAPIReqPerSecondLimit {
		return fmt.Errorf("config exceeds Akamai's requests per second limit (%d requests) by %d",
			akamaiAPIReqPerSecondLimit, requestsPerSecond-akamaiAPIReqPerSecondLimit)
	}

	// Purge no more than the 200 URLs we’re allotted each second.
	urlsPurgedPerSecond := requestsPerSecond * (t.ResponsesPerBatch * 3)
	if urlsPurgedPerSecond > akamaiURLsPerSecondLimit {
		return fmt.Errorf("config exceeds Akamai's URLs per second limit (%d URLs) by %d",
			akamaiURLsPerSecondLimit, urlsPurgedPerSecond-akamaiURLsPerSecondLimit)
	}
	return nil
}

type Config struct {
	AkamaiPurger struct {
		cmd.ServiceConfig

		// PurgeInterval is the duration waited between dispatching an Akamai
		// purge request containing 'DepracatedResponsesPerBatch' * 3 URLs.
		// Deprecated: TODO(#6003) this field is can be removed in favor of the
		// 'PurgeBatchEvery' field of the 'Throughput' struct.
		PurgeInterval cmd.ConfigDuration

		// MaxQueueSize is the maximum size of the purger queue. If this value
		// isn't provided it will default to `defaultQueueSize`.
		MaxQueueSize int

		BaseURL      string
		ClientToken  string
		ClientSecret string
		AccessToken  string
		V3Network    string

		// Throughput is a container for all throughput related akamai-purger
		// settings.
		Throughput Throughput

		// PurgeRetries is the maximum number of attempts that will be made to purge a
		// batch of URLs before the batch is added back to the queue.
		PurgeRetries int

		// PurgeRetryBackoff is the base duration that will be waited before
		// attempting to purge a batch of URLs which previously failed to be
		// purged.
		PurgeRetryBackoff cmd.ConfigDuration
	}
	Syslog  cmd.SyslogConfig
	Beeline cmd.BeelineConfig
}

// TODO(#6003) remove entirely.
func (c *Config) useDeprecatedSettings() {
	c.AkamaiPurger.Throughput.PurgeBatchEvery = c.AkamaiPurger.PurgeInterval
	c.AkamaiPurger.Throughput.ResponsesPerBatch = DeprecatedResponsesPerBatch
}

// akamaiPurger is a mutex protected container for a gRPC server which receives
// requests to purge the URLs associated with OCSP responses cached by Akamai,
// stores these URLs as a slice in an inner slice, and dispatches them to
// Akamai's Fast Purge API in batches.
type akamaiPurger struct {
	sync.Mutex
	akamaipb.UnimplementedAkamaiPurgerServer

	// toPurge functions as a queue where each entry contains the three OCSP response URLs
	// associated with a given certificate.
	toPurge      [][]string
	maxQueueSize int
	client       *akamai.CachePurgeClient
	log          blog.Logger
}

func (ap *akamaiPurger) len() int {
	ap.Lock()
	defer ap.Unlock()
	return len(ap.toPurge)
}

func (ap *akamaiPurger) purge() error {
	ap.Lock()
	queueEntries := ap.toPurge[:]
	ap.toPurge = [][]string{}
	ap.Unlock()
	if len(queueEntries) == 0 {
		return nil
	}

	stoppedAt, err := ap.client.Purge(queueEntries)
	if err != nil {
		ap.Lock()

		// Add the remaining queue entries back, but at the end of the queue. If somehow
		// there's a URL which repeatedly results in error, it won't block the
		// entire queue, only a single batch.
		ap.toPurge = append(ap.toPurge, queueEntries[stoppedAt:]...)
		ap.Unlock()
		ap.log.Errf("Failed to purge cached OCSP responses for %d certificates: %s", len(queueEntries), err)
		return err
	}
	return nil
}

// Purge is an exported gRPC method which receives purge requests and appends
// them to the queue.
func (ap *akamaiPurger) Purge(ctx context.Context, req *akamaipb.PurgeRequest) (*emptypb.Empty, error) {
	ap.Lock()
	defer ap.Unlock()
	if len(ap.toPurge) >= ap.maxQueueSize {
		return nil, errors.New("akamai-purger queue too large")
	}
	ap.toPurge = append(ap.toPurge, req.Urls)
	return &emptypb.Empty{}, nil
}

func main() {
	daemonFlags := flag.NewFlagSet("daemon", flag.ExitOnError)
	grpcAddr := daemonFlags.String("addr", "", "gRPC listen address override")
	debugAddr := daemonFlags.String("debug-addr", "", "Debug server address override")
	configFile := daemonFlags.String("config", "", "File path to the configuration file for this service")

	manualFlags := flag.NewFlagSet("manual", flag.ExitOnError)
	manualConfigFile := manualFlags.String("config", "", "File path to the configuration file for this service")
	tag := manualFlags.String("tag", "", "Single cache tag to purge")
	tagFile := manualFlags.String("tag-file", "", "File containing cache tags to purge, one per line")

	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		daemonFlags.PrintDefaults()
		fmt.Fprintln(os.Stderr, "OR:")
		fmt.Fprintf(os.Stderr, "%s manual <flags>\n", os.Args[0])
		manualFlags.PrintDefaults()
		os.Exit(1)
	}

	// Check if the purger is being started in daemon (URL purging gRPC service)
	// or manual (ad-hoc tag purging) mode.
	var manualMode bool
	if os.Args[1] == "manual" {
		manualMode = true
		_ = manualFlags.Parse(os.Args[2:])
		if *configFile == "" {
			manualFlags.Usage()
			os.Exit(1)
		}
		if *tag == "" && *tagFile == "" {
			cmd.Fail("Must specify one of --tag or --tag-file for manual purge")
		} else if *tag != "" && *tagFile != "" {
			cmd.Fail("Cannot specify both of --tag and --tag-file for manual purge")
		}
		configFile = manualConfigFile
	} else {
		_ = daemonFlags.Parse(os.Args[1:])
		if *configFile == "" {
			daemonFlags.Usage()
			os.Exit(1)
		}
	}

	var c Config
	err := cmd.ReadConfigFile(*configFile, &c)
	cmd.FailOnError(err, "Reading JSON config file into config structure")

	// Make references to the service config cleaner.
	apc := &c.AkamaiPurger

	if *grpcAddr != "" {
		apc.GRPC.Address = *grpcAddr
	}
	if *debugAddr != "" {
		apc.DebugAddr = *debugAddr
	}

	bc, err := c.Beeline.Load()
	cmd.FailOnError(err, "Failed to load Beeline config")
	beeline.Init(bc)
	defer beeline.Close()

	scope, logger := cmd.StatsAndLogging(c.Syslog, apc.DebugAddr)
	defer logger.AuditPanic()
	logger.Info(cmd.VersionString())

	// TODO(#6003) This block satisfies our deployability guidelines and can be
	// removed entirely once the 'purgeInterval' key has been removed from all
	// staging and production configuration.
	usingDeprecatedThroughput := apc.PurgeInterval.Duration != 0
	usingNewThroughput := apc.Throughput != Throughput{}
	if usingDeprecatedThroughput && usingNewThroughput {
		cmd.Fail("Config cannot specify both 'throughput': {...} AND 'purgeInterval'")
	}
	if usingDeprecatedThroughput && !usingNewThroughput {
		c.useDeprecatedSettings()
	}

	// When the operator hasn't specified any throughput settings, use the
	// optimized defaults. TODO(#6003) remove 'usingDeprecatedThroughput'.
	if !usingDeprecatedThroughput && !usingNewThroughput {
		apc.Throughput.useOptimizedDefaults()
	}
	cmd.FailOnError(apc.Throughput.validate(), "")

	if apc.MaxQueueSize == 0 {
		apc.MaxQueueSize = defaultQueueSize
	}

	ccu, err := akamai.NewCachePurgeClient(
		apc.BaseURL,
		apc.ClientToken,
		apc.ClientSecret,
		apc.AccessToken,
		apc.V3Network,
		apc.Throughput.PurgeBatchEvery.Duration,
		apc.Throughput.ResponsesPerBatch,
		apc.PurgeRetries,
		apc.PurgeRetryBackoff.Duration,
		logger,
		scope,
	)
	cmd.FailOnError(err, "Failed to setup Akamai CCU client")

	ap := &akamaiPurger{
		maxQueueSize: apc.MaxQueueSize,
		client:       ccu,
		log:          logger,
	}

	var gaugePurgeQueueLength = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "ccu_purge_queue_length",
			Help: "The length of the akamai-purger queue. Captured on each prometheus scrape.",
		},
		func() float64 { return float64(ap.len()) },
	)
	scope.MustRegister(gaugePurgeQueueLength)

	if manualMode {
		manualPurge(ccu, *tag, *tagFile, logger)
	} else {
		daemon(c, ap, logger, scope)
	}
}

// manualPurge is called ad-hoc to purge either a single tag, or a batch of tags,
// passed on the CLI. All tags will be added to a single request, please ensure
// that you don't violate the Fast-Purge API limits for tags detailed here:
// https://techdocs.akamai.com/purge-cache/reference/rate-limiting
func manualPurge(purgeClient *akamai.CachePurgeClient, tag, tagFile string, logger blog.Logger) {
	var tags []string
	if tag != "" {
		tags = []string{tag}
	} else {
		contents, err := os.ReadFile(tagFile)
		cmd.FailOnError(err, fmt.Sprintf("While reading %q", tagFile))
		tags = strings.Split(string(contents), "\n")
	}

	err := purgeClient.PurgeTags(tags)
	cmd.FailOnError(err, "Purging tags")
}

// daemon initializes the akamai-purger gRPC service.
func daemon(c Config, ap *akamaiPurger, logger blog.Logger, scope prometheus.Registerer) {
	clk := cmd.Clock()

	tlsConfig, err := c.AkamaiPurger.TLS.Load()
	cmd.FailOnError(err, "tlsConfig config")

	stop, stopped := make(chan bool, 1), make(chan bool, 1)
	ticker := time.NewTicker(c.AkamaiPurger.Throughput.PurgeBatchEvery.Duration)
	go func() {
	loop:
		for {
			select {
			case <-ticker.C:
				_ = ap.purge()
			case <-stop:
				break loop
			}
		}

		// As we may have missed a tick by calling ticker.Stop() and
		// writing to the stop channel call ap.purge one last time just
		// in case there is anything that still needs to be purged.
		queueLen := ap.len()
		if queueLen > 0 {
			logger.Infof("Shutting down; purging %d queue entries before exit.", queueLen)
			err := ap.purge()
			cmd.FailOnError(err, fmt.Sprintf("Shutting down; failed to purge %d queue entries before exit", queueLen))
			logger.Infof("Shutting down; finished purging %d queue entries.", queueLen)
		} else {
			logger.Info("Shutting down; queue is already empty.")
		}
		stopped <- true
	}()

	serverMetrics := bgrpc.NewServerMetrics(scope)

	grpcSrv, l, err := bgrpc.NewServer(c.AkamaiPurger.GRPC, tlsConfig, serverMetrics, clk)
	cmd.FailOnError(err, "Unable to setup Akamai purger gRPC server")

	akamaipb.RegisterAkamaiPurgerServer(grpcSrv, ap)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(grpcSrv, hs)

	go cmd.CatchSignals(logger, func() {
		hs.Shutdown()
		grpcSrv.GracefulStop()

		// Stop the ticker and signal that we want to shutdown by writing to the
		// stop channel. We wait 15 seconds for any remaining URLs to be emptied
		// from the current queue, if we pass that deadline we exit early.
		ticker.Stop()
		stop <- true
		select {
		case <-time.After(time.Second * 15):
			cmd.Fail("Timed out waiting for purger to finish work")
		case <-stopped:
		}
	})
	err = cmd.FilterShutdownErrors(grpcSrv.Serve(l))
	cmd.FailOnError(err, "akamai-purger gRPC service failed")

	// When we get a SIGTERM, we will exit from grpcSrv.Serve as soon as all
	// extant RPCs have been processed, but we want the process to stick around
	// while we still have a goroutine purging the last elements from the queue.
	// Once that's done, CatchSignals will call os.Exit().
	select {}
}

func init() {
	cmd.RegisterCommand("akamai-purger", main)
}
