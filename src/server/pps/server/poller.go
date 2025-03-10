package server

import (
	"context"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	kube_err "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kube_watch "k8s.io/apimachinery/pkg/watch"

	"github.com/pachyderm/pachyderm/v2/src/internal/backoff"
	"github.com/pachyderm/pachyderm/v2/src/internal/errors"
	"github.com/pachyderm/pachyderm/v2/src/internal/ppsdb"
	"github.com/pachyderm/pachyderm/v2/src/internal/watch"
	"github.com/pachyderm/pachyderm/v2/src/pps"
)

const pollBackoffTime = 2 * time.Second

// startPipelinePoller starts a new goroutine running pollPipelines
func (m *ppsMaster) startPipelinePoller() {
	m.pollPipelinesMu.Lock()
	defer m.pollPipelinesMu.Unlock()
	m.pollCancel = startMonitorThread(m.masterCtx, "pollPipelines", m.pollPipelines)
}

func (m *ppsMaster) cancelPipelinePoller() {
	m.pollPipelinesMu.Lock()
	defer m.pollPipelinesMu.Unlock()
	if m.pollCancel != nil {
		m.pollCancel()
		m.pollCancel = nil
	}
}

// startPipelinePodsPoller starts a new goroutine running pollPipelinePods
func (m *ppsMaster) startPipelinePodsPoller() {
	m.pollPipelinesMu.Lock()
	defer m.pollPipelinesMu.Unlock()
	m.pollPodsCancel = startMonitorThread(m.masterCtx, "pollPipelinePods", m.pollPipelinePods)
}

func (m *ppsMaster) cancelPipelinePodsPoller() {
	m.pollPipelinesMu.Lock()
	defer m.pollPipelinesMu.Unlock()
	if m.pollPodsCancel != nil {
		m.pollPodsCancel()
		m.pollPodsCancel = nil
	}
}

// startPipelineDBPoller starts a new goroutine running watchPipelines
func (m *ppsMaster) startPipelineWatcher() {
	m.pollPipelinesMu.Lock()
	defer m.pollPipelinesMu.Unlock()
	m.watchCancel = startMonitorThread(m.masterCtx, "watchPipelines", m.watchPipelines)
}

func (m *ppsMaster) cancelPipelineWatcher() {
	m.pollPipelinesMu.Lock()
	defer m.pollPipelinesMu.Unlock()
	if m.watchCancel != nil {
		m.watchCancel()
		m.watchCancel = nil
	}
}

//////////////////////////////////////////////////////////////////////////////
//                     PollPipelines Definition                             //
// - As in monitor.go, functions below should not call functions above, to  //
// avoid reentrancy deadlock.                                               //
//////////////////////////////////////////////////////////////////////////////

// pollPipelines generates regular updateEv and deleteEv events for each
// pipeline and sends them to ppsMaster.Run(). By scanning the database and k8s
// regularly and generating events for them, it prevents pipelines from getting
// orphaned.
func (m *ppsMaster) pollPipelines(ctx context.Context) {
	dbPipelines := map[string]bool{}
	if err := backoff.RetryUntilCancel(ctx, backoff.MustLoop(func() error {
		if len(dbPipelines) == 0 {
			// 1. Get the current set of pipeline RCs.
			//
			// We'll delete any RCs that don't correspond to a live pipeline after
			// querying the database to determine the set of live pipelines, but we
			// query k8s first to avoid a race (if we were to query the database
			// first, and CreatePipeline(foo) were to run between querying the
			// database and querying k8s, then we might delete the RC for brand-new
			// pipeline 'foo'). Even if we do delete a live pipeline's RC, it'll be
			// fixed in the next cycle)
			kc := m.a.env.KubeClient.CoreV1().ReplicationControllers(m.a.env.Config.Namespace)
			rcs, err := kc.List(ctx, metav1.ListOptions{
				LabelSelector: "suite=pachyderm,pipelineName",
			})
			if err != nil {
				// No sensible error recovery here (e.g .if we can't reach k8s). We'll
				// keep going, and just won't delete any RCs this round.
				log.Errorf("error polling pipeline RCs: %v", err)
			}

			// 2. Replenish 'dbPipelines' with the set of pipelines currently in
			// the database. Note that there may be zero, and dbPipelines may be empty
			if err := m.a.listPipelineInfo(ctx, nil, 0,
				func(ptr *pps.PipelineInfo) error {
					dbPipelines[ptr.Pipeline.Name] = true
					return nil
				}); err != nil {
				// listPipelineInfo results (dbPipelines) are used by all remaining
				// steps, so if that didn't work, start over and try again
				dbPipelines = map[string]bool{}
				return errors.Wrap(err, "error polling pipelines")
			}

			// 3. Generate a delete event for orphaned RCs
			if rcs != nil {
				for _, rc := range rcs.Items {
					pipeline, ok := rc.Labels["pipelineName"]
					if !ok {
						return errors.New("'pipelineName' label missing from rc " + rc.Name)
					}
					if !dbPipelines[pipeline] {
						m.eventCh <- &pipelineEvent{eventType: deleteEv, pipeline: pipeline}
					}
				}
			}

			// 4. Retry if there are no pipelines to read/write
			if len(dbPipelines) == 0 {
				return backoff.ErrContinue
			}
		}

		// Generate one event for a pipeline (to trigger the pipeline controller)
		// and remove this pipeline from dbPipelines. Always choose the
		// lexicographically smallest pipeline so that pipelines are always
		// traversed in the same order and the period between polls is stable across
		// all pipelines.
		var pipeline string
		for p := range dbPipelines {
			if pipeline == "" || p < pipeline {
				pipeline = p
			}
		}

		// always rm 'pipeline', to advance loop
		delete(dbPipelines, pipeline)

		// generate a pipeline event for 'pipeline'
		log.Debugf("PPS master: polling pipeline %q", pipeline)
		select {
		case m.eventCh <- &pipelineEvent{eventType: writeEv, pipeline: pipeline}:
			break
		case <-ctx.Done():
			break
		}

		// 5. move to next pipeline (after 2s sleep)
		return nil
	}), backoff.NewConstantBackOff(pollBackoffTime),
		backoff.NotifyContinue("pollPipelines"),
	); err != nil && ctx.Err() == nil {
		log.Fatalf("pollPipelines is exiting prematurely which should not happen (error: %v); restarting container...", err)
	}
}

// pollPipelinePods creates a kubernetes watch, and for each event:
//   1) Checks if the event concerns a Pod
//   2) Checks if the Pod belongs to a pipeline (pipelineName annotation is set)
//   3) Checks if the Pod is failing
// If all three conditions are met, then the pipline (in 'pipelineName') is set
// to CRASHING
func (m *ppsMaster) pollPipelinePods(ctx context.Context) {
	if err := backoff.RetryUntilCancel(ctx, backoff.MustLoop(func() error {
		kubePipelineWatch, err := m.a.env.KubeClient.CoreV1().Pods(m.a.namespace).Watch(
			ctx,
			metav1.ListOptions{
				LabelSelector: metav1.FormatLabelSelector(metav1.SetAsLabelSelector(
					map[string]string{
						"component": "worker",
					})),
				Watch: true,
			})
		if err != nil {
			return errors.Wrap(err, "failed to watch kubernetes pods")
		}
		defer kubePipelineWatch.Stop()
	WatchLoop:
		for {
			select {
			case <-ctx.Done():
				return nil
			case event := <-kubePipelineWatch.ResultChan():
				// if we get an error we restart the watch
				if event.Type == kube_watch.Error {
					return errors.Wrap(kube_err.FromObject(event.Object), "error while watching kubernetes pods")
				} else if event.Type == "" {
					// k8s watches seem to sometimes get stuck in a loop returning events
					// with Type = "". We treat these as errors as otherwise we get an
					// endless stream of them and can't do anything.
					return errors.New("error while watching kubernetes pods: empty event type")
				}
				pod, ok := event.Object.(*v1.Pod)
				if !ok {
					continue // irrelevant event
				}
				if pod.Status.Phase == v1.PodFailed {
					log.Errorf("pod failed because: %s", pod.Status.Message)
				}
				crashPipeline := func(reason string) error {
					pipelineName := pod.ObjectMeta.Annotations["pipelineName"]
					pipelineVersion, versionErr := strconv.Atoi(pod.ObjectMeta.Annotations["pipelineVersion"])
					if versionErr != nil {
						return errors.Wrapf(err, "couldn't find pipeline rc version")
					}
					var pipelineInfo pps.PipelineInfo
					if err := m.a.pipelines.ReadOnly(ctx).GetUniqueByIndex(
						ppsdb.PipelinesVersionIndex,
						ppsdb.VersionKey(pipelineName, uint64(pipelineVersion)),
						&pipelineInfo); err != nil {
						return errors.Wrapf(err, "couldn't retrieve pipeline information")
					}
					return m.setPipelineCrashing(ctx, pipelineInfo.SpecCommit, reason)
				}
				for _, status := range pod.Status.ContainerStatuses {
					if status.State.Waiting != nil && failures[status.State.Waiting.Reason] {
						if err := crashPipeline(status.State.Waiting.Message); err != nil {
							return errors.Wrap(err, "error moving pipeline to CRASHING")
						}
						continue WatchLoop
					}
				}
				for _, condition := range pod.Status.Conditions {
					if condition.Type == v1.PodScheduled &&
						condition.Status != v1.ConditionTrue && failures[condition.Reason] {
						if err := crashPipeline(condition.Message); err != nil {
							return errors.Wrap(err, "error moving pipeline to CRASHING")
						}
						continue WatchLoop
					}
				}
			}
		}
	}), backoff.NewInfiniteBackOff(), backoff.NotifyContinue("pollPipelinePods"),
	); err != nil && ctx.Err() == nil {
		log.Fatalf("pollPipelinePods is exiting prematurely which should not happen (error: %v); restarting container...", err)
	}
}

// watchPipelines watches the 'pipelines' collection in the database and sends
// writeEv and deleteEv events to the PPS master when it sees them.
//
// watchPipelines is unlike the other poll and monitor goroutines in that it sees
// the result of other poll/monitor goroutines' writes. For example, when
// pollPipelinePods (above) observes that a pipeline is crashing and updates its
// state in the database, the flow for starting monitorPipelineCrashing is:
//
//  k8s watch ─> pollPipelinePods  ╭───> watchPipelines    ╭──> m.run()
//                      │          │            │          │      │
//                      ↓          │            ↓          │      ↓
//                   db write──────╯       m.eventCh ──────╯   m.step()
//
// most of the other poll/monitor goroutines actually go through pollPipelines
// (by writing to the database, which is then observed by the watch below)
func (m *ppsMaster) watchPipelines(ctx context.Context) {
	if err := backoff.RetryUntilCancel(ctx, backoff.MustLoop(func() error {
		// TODO(msteffen) request only keys, since pipeline_controller.go reads
		// fresh values for each event anyway
		pipelineWatcher, err := m.a.pipelines.ReadOnly(ctx).Watch()
		if err != nil {
			return errors.Wrapf(err, "error creating watch")
		}
		defer pipelineWatcher.Close()

		for event := range pipelineWatcher.Watch() {
			if event.Err != nil {
				return errors.Wrapf(event.Err, "event err")
			}
			pipelineName, _, err := ppsdb.ParsePipelineKey(string(event.Key))
			if err != nil {
				return errors.Wrap(err, "bad watch event key")
			}
			switch event.Type {
			case watch.EventPut:
				m.eventCh <- &pipelineEvent{
					eventType: writeEv,
					pipeline:  pipelineName,
					timestamp: time.Unix(event.Rev, 0),
				}
			case watch.EventDelete:
				m.eventCh <- &pipelineEvent{
					eventType: deleteEv,
					pipeline:  pipelineName,
					timestamp: time.Unix(event.Rev, 0),
				}
			}
		}
		return nil // reset until ctx is cancelled (RetryUntilCancel)
	}), &backoff.ZeroBackOff{}, backoff.NotifyContinue("watchPipelines"),
	); err != nil && ctx.Err() == nil {
		log.Fatalf("watchPipelines is exiting prematurely which should not happen (error: %v); restarting container...", err)
	}
}
