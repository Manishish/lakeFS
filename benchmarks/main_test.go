package benchmarks

import (
	"bytes"
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/go-openapi/runtime"
	"github.com/go-openapi/swag"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/prom2json"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"github.com/thanhpk/randstr"
	genclient "github.com/treeverse/lakefs/api/gen/client"
	"github.com/treeverse/lakefs/api/gen/client/objects"
	"github.com/treeverse/lakefs/api/gen/client/repositories"
	"github.com/treeverse/lakefs/api/gen/models"
	"github.com/treeverse/lakefs/logging"
	"github.com/treeverse/lakefs/testutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	logger logging.Logger
	client *genclient.Lakefs
	svc    *s3.S3
)

func TestMain(m *testing.M) {
	//benchmarkTests := flag.Bool("benchmark-tests", false, "Run benchmark tests")
	//flag.Parse()
	//if !*benchmarkTests {
	//	os.Exit(0)
	//}

	viper.SetDefault("parallelism_level", 500)
	viper.SetDefault("files_amount", 10000)
	viper.SetDefault("global_timeout", 30*time.Minute)

	logger, client, svc = testutil.SetupTestingEnv("benchmark", "lakefs-benchmarking")
	logger.Info("Setup succeeded, running the tests")

	if code := m.Run(); code != 0 {
		logger.Info("Tests run failed")
		os.Exit(code)
	}

	scrapePrometheus()
}

var monitoredOps = map[string]bool{
	"getObject":    true,
	"uploadObject": true,
}

func scrapePrometheus() {
	lakefsEndpoint := viper.GetString("endpoint_url")
	resp, err := http.DefaultClient.Get(lakefsEndpoint + "/metrics")
	if err != nil {
		panic(err)
	}

	ch := make(chan *dto.MetricFamily)
	go func() { _ = prom2json.ParseResponse(resp, ch) }()
	metrics := []*dto.Metric{}

	for {
		a, ok := <-ch
		if !ok {
			break
		}

		if *a.Name == "api_request_duration_seconds" {
			for _, m := range a.Metric {
				for _, label := range m.Label {
					if *label.Name == "operation" && monitoredOps[*label.Value] {
						metrics = append(metrics, m)
					}
				}
			}
		}
	}

	for _, m := range metrics {
		fmt.Printf("%v\n", *m)
	}
}

const (
	contentSuffixLength = 32
	//contentLength       = 128 * 1024
	contentLength = 1 * 1024
)

func TestBenchmarkLakeFS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), viper.GetDuration("global_timeout"))
	defer cancel()

	ns := viper.GetString("storage_namespace")
	repoName := strings.ToLower(t.Name())
	logger.WithFields(logging.Fields{
		"repository":        repoName,
		"storage_namespace": ns,
		"name":              repoName,
	}).Debug("Create repository for test")
	_, err := client.Repositories.CreateRepository(repositories.NewCreateRepositoryParamsWithContext(ctx).
		WithRepository(&models.RepositoryCreation{
			DefaultBranch:    "master",
			ID:               swag.String(repoName),
			StorageNamespace: swag.String(ns),
		}), nil)
	require.NoErrorf(t, err, "failed to create repository '%s', storage '%s'", t.Name(), ns)

	parallelism := viper.GetInt("parallelism_level")
	filesAmount := viper.GetInt("files_amount")

	contentPrefix := randstr.Hex(contentLength - contentSuffixLength)
	failed := doInParallel(ctx, repoName, parallelism, filesAmount, contentPrefix, uploader)
	logger.WithField("failedCount", failed).Info("Finished uploading files")

	failed = doInParallel(ctx, repoName, parallelism, filesAmount, "", reader)
	logger.WithField("failedCount", failed).Info("Finished reading files")

}

func doInParallel(ctx context.Context, repoName string, level, filesAmount int, contentPrefix string, do func(context.Context, chan string, string, string) int) int {
	filesCh := make(chan string, level)
	wg := sync.WaitGroup{}
	var failed int64

	for i := 0; i < level; i++ {
		go func() {
			wg.Add(1)
			fail := do(ctx, filesCh, repoName, contentPrefix)
			atomic.AddInt64(&failed, int64(fail))
			wg.Done()
		}()
	}

	for i := 1; i <= filesAmount; i++ {
		filesCh <- strconv.Itoa(i)
	}

	close(filesCh)
	wg.Wait()

	return int(failed)
}

func uploader(ctx context.Context, ch chan string, repoName, contentPrefix string) int {
	failed := 0
	for {
		select {
		case <-ctx.Done():
			return failed
		case file, ok := <-ch:
			if !ok {
				// channel closed
				return failed
			}

			// Making sure content isn't duplicated to avoid dedup mechanisms in lakeFS
			content := contentPrefix + randstr.Hex(contentSuffixLength)
			contentReader := runtime.NamedReader("content", strings.NewReader(content))

			if err := linearRetry(func() error {
				_, err := client.Objects.UploadObject(
					objects.NewUploadObjectParamsWithContext(ctx).
						WithRepository(repoName).
						WithBranch("master").
						WithPath(file).
						WithContent(contentReader), nil)
				return err
			}); err != nil {
				failed++
				logger.WithField("fileNum", file).Error("Failed uploading file")
			}
		}
	}
}

func reader(ctx context.Context, ch chan string, repoName, _ string) int {
	failed := 0
	for {
		select {
		case <-ctx.Done():
			return failed
		case file, ok := <-ch:
			if !ok {
				// channel closed
				return failed
			}

			if err := linearRetry(func() error {
				var b bytes.Buffer
				_, err := client.Objects.GetObject(
					objects.NewGetObjectParamsWithContext(ctx).
						WithRepository(repoName).
						WithRef("master").
						WithPath(file), nil, &b)
				return err
			}); err != nil {
				failed++
				logger.WithField("fileNum", file).Error("Failed reading file")
			}
		}
	}
}

const (
	tries        = 3
	retryTimeout = 200 * time.Millisecond
)

func linearRetry(do func() error) error {
	var err error
	for i := 1; i <= tries; i++ {
		if err = do(); err == nil {
			return nil
		}

		if i != tries {
			// skip sleep in the last iteration
			time.Sleep(retryTimeout)
		}
	}
	return err
}
