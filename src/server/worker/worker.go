package worker

import (
	"context"
	"fmt"
	"path"
	"time"

	"github.com/gogo/protobuf/types"
	"golang.org/x/sync/errgroup"

	"github.com/pachyderm/pachyderm/v2/src/auth"
	"github.com/pachyderm/pachyderm/v2/src/client"
	"github.com/pachyderm/pachyderm/v2/src/internal/backoff"
	"github.com/pachyderm/pachyderm/v2/src/internal/dlock"
	"github.com/pachyderm/pachyderm/v2/src/internal/errors"
	"github.com/pachyderm/pachyderm/v2/src/internal/ppsutil"
	"github.com/pachyderm/pachyderm/v2/src/internal/serviceenv"
	"github.com/pachyderm/pachyderm/v2/src/pps"
	"github.com/pachyderm/pachyderm/v2/src/server/worker/driver"
	"github.com/pachyderm/pachyderm/v2/src/server/worker/logs"
	"github.com/pachyderm/pachyderm/v2/src/server/worker/pipeline/service"
	"github.com/pachyderm/pachyderm/v2/src/server/worker/pipeline/spout"
	"github.com/pachyderm/pachyderm/v2/src/server/worker/pipeline/transform"
	"github.com/pachyderm/pachyderm/v2/src/server/worker/server"
	"github.com/pachyderm/pachyderm/v2/src/server/worker/stats"
)

const (
	masterLockPath = "_master_worker_lock"
)

// The Worker object represents
type Worker struct {
	APIServer *server.APIServer // Provides rpcs for other nodes in the cluster
	driver    driver.Driver     // Provides common functions used by worker code
	status    *transform.Status // An interface for inspecting and canceling the actively running task
}

// NewWorker constructs a Worker object that provides all worker functionality:
//  1. a master goroutine that attempts to obtain the master lock for the pipeline workers and direct jobs
//  2. a worker goroutine that gets tasks from the master and processes them
//  3. an api server that serves requests for status or cross-worker communication
//  4. a driver that provides common functionality between the above components
func NewWorker(
	env serviceenv.ServiceEnv,
	pachClient *client.APIClient,
	pipelineInfo *pps.PipelineInfo,
	rootPath string,
) (*Worker, error) {
	stats.InitPrometheus()

	driver, err := driver.NewDriver(
		env,
		pachClient,
		pipelineInfo,
		rootPath,
	)
	if err != nil {
		return nil, err
	}

	if pipelineInfo.Details.Transform.Image != "" && pipelineInfo.Details.Transform.Cmd == nil {
		ppsutil.FailPipeline(env.Context(), env.GetDBClient(), driver.Pipelines(),
			pipelineInfo.SpecCommit,
			"nothing to run: no transform.cmd")
	}

	worker := &Worker{
		driver: driver,
		status: &transform.Status{},
	}

	worker.APIServer = server.NewAPIServer(driver, worker.status, env.Config().PodName)

	go worker.master(env)
	go worker.worker()
	return worker, nil
}

func (w *Worker) worker() {
	ctx := w.driver.PachClient().Ctx()
	logger := logs.NewStatlessLogger(w.driver.PipelineInfo())

	backoff.RetryUntilCancel(ctx, func() error {
		eg, ctx := errgroup.WithContext(ctx)
		driver := w.driver.WithContext(ctx)

		// Process any tasks that the master creates.
		eg.Go(func() error {
			return driver.NewTaskSource().Iterate(
				ctx,
				func(ctx context.Context, input *types.Any) (*types.Any, error) {
					driver := w.driver.WithContext(ctx)
					return transform.Worker(driver, logger, input, w.status)
				},
			)
		})

		return eg.Wait()
	}, backoff.NewConstantBackOff(200*time.Millisecond), func(err error, d time.Duration) error {
		if st, ok := err.(errors.StackTracer); ok {
			logger.Logf("worker failed, retrying in %v:\n%s\n%+v", d, err, st.StackTrace())
		} else {
			logger.Logf("worker failed, retrying in %v:\n%s", d, err)
		}
		return nil
	})
}

func (w *Worker) master(env serviceenv.ServiceEnv) {
	pipelineInfo := w.driver.PipelineInfo()
	logger := logs.NewMasterLogger(pipelineInfo)
	lockPath := path.Join(env.Config().PPSEtcdPrefix, masterLockPath, pipelineInfo.Pipeline.Name, pipelineInfo.Details.Salt)
	masterLock := dlock.NewDLock(env.GetEtcdClient(), lockPath)

	b := backoff.NewInfiniteBackOff()
	// Setting a high backoff so that when this master fails, the other
	// workers are more likely to become the master.
	// Also, we've observed race conditions where StopPipeline would cause
	// a master to restart before it's deleted.  PPS would then get confused
	// by the restart and create the workers again, because the restart would
	// bring the pipeline state from PAUSED to RUNNING.  By setting a high
	// retry interval, the master would be deleted before it gets a chance
	// to restart.
	b.InitialInterval = 10 * time.Second
	backoff.RetryNotify(func() error {
		// We use pachClient.Ctx here because it contains auth information.
		ctx, cancel := context.WithCancel(w.driver.PachClient().Ctx())
		defer cancel() // make sure that everything this loop might spawn gets cleaned up
		ctx, err := masterLock.Lock(ctx)
		if err != nil {
			return errors.Wrap(err, "locking master lock")
		}
		defer masterLock.Unlock(ctx)

		// Create a new driver that uses a new cancelable pachClient
		return runSpawner(w.driver.WithContext(ctx), logger)
	}, b, func(err error, d time.Duration) error {
		if auth.IsErrNotAuthorized(err) {
			logger.Logf("failing %q due to auth rejection", pipelineInfo.Pipeline.Name)
			return ppsutil.FailPipeline(
				w.driver.PachClient().Ctx(),
				env.GetDBClient(),
				w.driver.Pipelines(),
				pipelineInfo.SpecCommit,
				"worker master could not access output repo to watch for new commits",
			)
		}
		logger.Logf("master: error running the master process, retrying in %v: %+v", d, err)
		return nil
	})
}

type spawnerFunc func(driver.Driver, logs.TaggedLogger) error

// Run runs the spawner for a given pipeline.  This switches between several
// underlying functions based on the configuration in pipelineInfo (e.g. if
// it is a service, a spout, or a transform pipeline).
func runSpawner(driver driver.Driver, logger logs.TaggedLogger) error {
	pipelineType, runFn := func() (string, spawnerFunc) {
		switch {
		case driver.PipelineInfo().Details.Service != nil:
			return "service", service.Run
		case driver.PipelineInfo().Details.Spout != nil:
			return "spout", spout.Run
		default:
			return "transform", transform.Run
		}
	}()

	return logger.LogStep(fmt.Sprintf("%v spawner process", pipelineType), func() error {
		return runFn(driver, logger)
	})
}
