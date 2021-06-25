package renter

import (
	"context"
	"sync"
	"time"

	"github.com/opentracing/opentracing-go"
	"gitlab.com/SkynetLabs/skyd/build"
	"gitlab.com/SkynetLabs/skyd/skymodules"
	"go.sia.tech/siad/crypto"
	"go.sia.tech/siad/modules"
	"go.sia.tech/siad/types"

	"gitlab.com/NebulousLabs/errors"
)

const (
	// jobHasSectorPerformanceDecay defines how much the average performance is
	// decayed each time a new datapoint is added. The jobs use an exponential
	// weighted average.
	// jobHasSectorPerformanceDecay = 0.9

	// hasSectorBatchSize is the number of has sector jobs batched together upon
	// calling callNext.
	// This number is the result of empirical testing which determined that 13
	// requests can be batched together without increasing the required
	// upload or download bandwidth.
	hasSectorBatchSize = 13
)

// JobTime tracks multiple potential durations of a job. e.g. the p50, p90 and
// p99.
type JobTime []time.Duration

// ResolveTime turns a JobTime into a ResolveTime by giving it a start time.
func (jt JobTime) ResolveTime(start time.Time) ResolveTime {
	return ResolveTime{
		start: start,
		times: append(JobTime{}, jt...), // deep copy
	}
}

// ReadTime creates a new ReadTime given an existing ResolveTime and JobTime.
// The ResolveTime belongs to an unresolved job and the JobTime is the duration
// of the job that depends on it.
func (rt ResolveTime) ReadTime(jt JobTime) ReadTime {
	return ReadTime{
		rt:    rt,
		times: jt,
	}
}

// Min returns the smallest duration within the JobTime.
func (jt JobTime) Min() time.Duration {
	return jt[0]
}

// Max returns the largest duration within the JobTime.
func (jt JobTime) Max() time.Duration {
	return jt[len(jt)-1]
}

// ResolveTime is a JobTime with added start time. It's used to estimate when a
// given Job is expected to finish given the passed time since the start.
type ResolveTime struct {
	start time.Time
	times JobTime
}

// ReadTime is the time of a read job that depends on another job resolving
// before it can be launched.
type ReadTime struct {
	rt    ResolveTime
	times JobTime
}

// Duration returns the estimated duration of the read job. The larger the delay
// on the underlying unresolved job, the longer the duration of the read job.
func (rt ReadTime) Duration() time.Duration {
	_, i := rt.rt.time()
	return rt.times[i]
}

// Add adds another adds a resolve time to the curren time. The length of the
// durations must match.
func (rt ResolveTime) Add(jt JobTime) ResolveTime {
	if len(rt.times) != len(jt) {
		build.Critical("lengths of times don't match")
		return rt
	}
	rtNew := ResolveTime{
		times: append(JobTime{}, rt.times...),
		start: rt.start,
	}
	// TODO: this shouldn't just add like that. Instead it should use the
	// values from rt to determine what index to use and then add the values
	// from rt2.
	for i, t := range jt {
		rtNew.times[i] += t
	}
	return rtNew
}

// AddToJobTime adds some duration to the job time.
func (jt JobTime) AddToJobTime(d time.Duration) JobTime {
	jtNew := make(JobTime, len(jt))
	for i := range jtNew {
		jtNew[i] = jt[i] + d
	}
	return jtNew
}

// AddToJobTime adds some duration to the underlying job time.
func (rt *ResolveTime) AddToJobTime(d time.Duration) ResolveTime {
	rtNew := ResolveTime{
		times: rt.times.AddToJobTime(d),
		start: rt.start,
	}
	return rtNew
}

// Time returns the time we expect the task to resolve. It returns the closest
// expected time by going through the list of potential durations and choosing
// the lowest one that's still in the future. If no such time is found, the
// largest duration is chosen.
func (rt ResolveTime) Time() time.Time {
	t, _ := rt.time()
	return t
}

// time returns the time we expect the task to resolve. It returns the closest
// expected time by going through the list of potential durations and choosing
// the lowest one that's still in the future. If no such time is found, the
// largest duration is chosen.
func (rt ResolveTime) time() (time.Time, int) {
	if len(rt.times) == 0 {
		build.Critical("empty resolve time")
		return time.Time{}, 0
	}
	passedTime := time.Since(rt.start)
	for i, d := range rt.times {
		if passedTime < d {
			return rt.start.Add(d), i
		}
	}
	return rt.start.Add(rt.times.Max()), len(rt.times) - 1
}

// errEstimateAboveMax is returned if a HasSector job wasn't added due to the
// estimate exceeding the max.
var errEstimateAboveMax = errors.New("can't add job since estimate is above max timeout")

type (
	// jobHasSector contains information about a hasSector query.
	jobHasSector struct {
		staticSectors      []crypto.Hash
		staticResponseChan chan *jobHasSectorResponse

		staticPostExecutionHook func(*jobHasSectorResponse)
		once                    sync.Once

		staticSpan opentracing.Span

		*jobGeneric
	}

	// jobHasSectorBatch is a batch of has sector lookups.
	jobHasSectorBatch struct {
		staticJobs []*jobHasSector
	}

	// jobHasSectorQueue is a list of hasSector queries that have been assigned
	// to the worker.
	jobHasSectorQueue struct {
		// These variables contain an exponential weighted average of the
		// worker's recent performance for jobHasSectorQueue.
		staticDT *skymodules.DistributionTracker

		*jobGenericQueue
	}

	// jobHasSectorResponse contains the result of a hasSector query.
	jobHasSectorResponse struct {
		staticAvailables []bool
		staticErr        error

		// The worker is included in the response so that the caller can listen
		// on one channel for a bunch of workers and still know which worker
		// successfully found the sector root.
		staticWorker *worker

		// The time it took for this job to complete is included for debugging
		// purposes.
		staticJobTime time.Duration
	}
)

// callNext overwrites the generic call next and batches a certain number of has
// sector jobs together.
func (jq *jobHasSectorQueue) callNext() workerJob {
	var jobs []*jobHasSector

	for {
		if len(jobs) >= hasSectorBatchSize {
			break
		}
		next := jq.jobGenericQueue.callNext()
		if next == nil {
			break
		}
		j := next.(*jobHasSector)
		jobs = append(jobs, j)
	}
	if len(jobs) == 0 {
		return nil
	}

	return &jobHasSectorBatch{
		staticJobs: jobs,
	}
}

// newJobHasSector is a helper method to create a new HasSector job.
func (w *worker) newJobHasSector(ctx context.Context, responseChan chan *jobHasSectorResponse, roots ...crypto.Hash) *jobHasSector {
	return w.newJobHasSectorWithPostExecutionHook(ctx, responseChan, nil, roots...)
}

// newJobHasSectorWithPostExecutionHook is a helper method to create a new
// HasSector job with a post execution hook that is executed after the response
// is available but before sending it over the channel.
func (w *worker) newJobHasSectorWithPostExecutionHook(ctx context.Context, responseChan chan *jobHasSectorResponse, hook func(*jobHasSectorResponse), roots ...crypto.Hash) *jobHasSector {
	span, _ := opentracing.StartSpanFromContext(ctx, "HasSectorJob")
	return &jobHasSector{
		staticSectors:           roots,
		staticResponseChan:      responseChan,
		staticPostExecutionHook: hook,
		staticSpan:              span,
		jobGeneric:              newJobGeneric(ctx, w.staticJobHasSectorQueue, nil),
	}
}

// callDiscard will discard a job, sending the provided error.
func (j *jobHasSector) callDiscard(err error) {
	w := j.staticQueue.staticWorker()
	errLaunch := w.staticRenter.tg.Launch(func() {
		response := &jobHasSectorResponse{
			staticErr: errors.Extend(err, ErrJobDiscarded),

			staticWorker: w,
		}
		j.managedCallPostExecutionHook(response)
		select {
		case j.staticResponseChan <- response:
		case <-j.staticCtx.Done():
		case <-w.staticRenter.tg.StopChan():
		}
	})
	if errLaunch != nil {
		w.staticRenter.staticLog.Print("callDiscard: launch failed", err)
	}

	j.staticSpan.LogKV("callDiscard", err)
	j.staticSpan.SetTag("success", false)
	j.staticSpan.Finish()
}

// callDiscard discards all jobs within the batch.
func (j jobHasSectorBatch) callDiscard(err error) {
	for _, hsj := range j.staticJobs {
		hsj.callDiscard(err)
	}
}

// staticCanceled always returns false. A batched job never resides in the
// queue. It's constructed right before being executed.
func (j jobHasSectorBatch) staticCanceled() bool {
	return false
}

// staticGetMetadata return an empty struct. A batched has sector job doesn't
// contain any metadata.
func (j jobHasSectorBatch) staticGetMetadata() interface{} {
	return struct{}{}
}

// callExecute will run the has sector job.
func (j *jobHasSector) callExecute() {
	// Finish job span at the end.
	defer j.staticSpan.Finish()

	// Capture callExecute in new span.
	span := opentracing.StartSpan("callExecute", opentracing.ChildOf(j.staticSpan.Context()))
	defer span.Finish()

	batch := jobHasSectorBatch{
		staticJobs: []*jobHasSector{j},
	}
	batch.callExecute()
}

// callExecute will run the has sector job.
func (j jobHasSectorBatch) callExecute() {
	if len(j.staticJobs) == 0 {
		build.Critical("empty hasSectorBatch")
		return
	}

	start := time.Now()
	w := j.staticJobs[0].staticQueue.staticWorker()
	availables, err := j.managedHasSector()
	jobTime := time.Since(start)

	for i := range j.staticJobs {
		hsj := j.staticJobs[i]
		// Handle its span
		if err != nil {
			hsj.staticSpan.LogKV("error", err)
		}
		hsj.staticSpan.SetTag("success", err == nil)
		hsj.staticSpan.Finish()

		// Create the response.
		response := &jobHasSectorResponse{
			staticErr:     err,
			staticJobTime: jobTime,
			staticWorker:  w,
		}
		// If it was successful, attach the result.
		if err == nil {
			hsj.staticSpan.LogKV("availables", availables[i])
			response.staticAvailables = availables[i]
		}
		// Send the response.
		err2 := w.staticRenter.tg.Launch(func() {
			hsj.managedCallPostExecutionHook(response)
			select {
			case hsj.staticResponseChan <- response:
			case <-hsj.staticCtx.Done():
			case <-w.staticRenter.tg.StopChan():
			}
		})
		// Report success or failure to the queue.
		if err != nil {
			hsj.staticQueue.callReportFailure(err)
			continue
		}
		hsj.staticQueue.callReportSuccess()

		// Job was a success, update the performance stats on the queue.
		jq := hsj.staticQueue.(*jobHasSectorQueue)
		jq.callUpdateJobTimeMetrics(jobTime)
		if err2 != nil {
			w.staticRenter.staticLog.Println("callExecute: launch failed", err)
		}
	}
}

// callExpectedBandwidth returns the bandwidth that is expected to be consumed
// by the job.
func (j *jobHasSector) callExpectedBandwidth() (ul, dl uint64) {
	// sanity check
	if len(j.staticSectors) == 0 {
		build.Critical("expected bandwidth requested for a job that has no staticSectors set")
	}
	return hasSectorJobExpectedBandwidth(len(j.staticSectors))
}

// callExpectedBandwidth returns the bandwidth that is expected to be consumed
// by the job.
func (j jobHasSectorBatch) callExpectedBandwidth() (ul, dl uint64) {
	var totalSectors int
	for _, hsj := range j.staticJobs {
		// sanity check
		if len(hsj.staticSectors) == 0 {
			build.Critical("expected bandwidth requested for a job that has no staticSectors set")
		}
		totalSectors += len(hsj.staticSectors)
	}
	ul, dl = hasSectorJobExpectedBandwidth(totalSectors)
	return
}

// managedHasSector returns whether or not the host has a sector with given root
func (j *jobHasSectorBatch) managedHasSector() (results [][]bool, err error) {
	if len(j.staticJobs) == 0 {
		return nil, nil
	}

	w := j.staticJobs[0].staticQueue.staticWorker()
	// Create the program.
	pt := w.staticPriceTable().staticPriceTable
	pb := modules.NewProgramBuilder(&pt, 0) // 0 duration since HasSector doesn't depend on it.
	for _, hsj := range j.staticJobs {
		for _, sector := range hsj.staticSectors {
			pb.AddHasSectorInstruction(sector)
		}
	}
	program, programData := pb.Program()
	cost, _, _ := pb.Cost(true)

	// take into account bandwidth costs
	ulBandwidth, dlBandwidth := j.callExpectedBandwidth()
	bandwidthCost := modules.MDMBandwidthCost(pt, ulBandwidth, dlBandwidth)
	cost = cost.Add(bandwidthCost)

	// Execute the program and parse the responses.
	hasSectors := make([]bool, 0, len(program))
	var responses []programResponse
	responses, _, err = w.managedExecuteProgram(program, programData, types.FileContractID{}, categoryDownload, cost)
	if err != nil {
		return nil, errors.AddContext(err, "unable to execute program for has sector job")
	}
	for _, resp := range responses {
		if resp.Error != nil {
			return nil, errors.AddContext(resp.Error, "Output error")
		}
		hasSectors = append(hasSectors, resp.Output[0] == 1)
	}
	if len(responses) != len(program) {
		return nil, errors.New("received invalid number of responses but no error")
	}

	for _, hsj := range j.staticJobs {
		results = append(results, hasSectors[:len(hsj.staticSectors)])
		hasSectors = hasSectors[len(hsj.staticSectors):]
	}
	return results, nil
}

// callAddWithEstimate will add a job to the queue and return a timestamp for
// when the job is estimated to complete. An error will be returned if the job
// is not successfully queued.
func (jq *jobHasSectorQueue) callAddWithEstimate(j *jobHasSector, maxEstimate time.Duration) (ResolveTime, error) {
	jq.mu.Lock()
	defer jq.mu.Unlock()
	now := time.Now()
	estimate := jq.expectedJobTime()
	if estimate.Max() > maxEstimate {
		return ResolveTime{}, errEstimateAboveMax
	}
	j.externJobStartTime = now
	if !jq.add(j) {
		return ResolveTime{}, errors.New("unable to add job to queue")
	}
	return estimate.ResolveTime(now), nil
}

// callExpectedJobTime returns the expected amount of time that this job will
// take to complete.
//
// TODO: idealy we pass `numSectors` here and get the expected job time
// depending on the amount of instructions in the program.
func (jq *jobHasSectorQueue) callExpectedJobTime() JobTime {
	jq.mu.Lock()
	defer jq.mu.Unlock()
	return jq.expectedJobTime()
}

// callUpdateJobTimeMetrics takes a duration it took to fulfil that job and uses
// it to update the job performance metrics on the queue.
func (jq *jobHasSectorQueue) callUpdateJobTimeMetrics(jobTime time.Duration) {
	jq.mu.Lock()
	defer jq.mu.Unlock()
	jq.staticDT.AddDataPoint(jobTime)
}

var jobTimePercentiles = []float64{0.5, 0.6, 0.7, 0.8, 0.9, .99, .999}

// expectedJobTime will return the amount of time that a job is expected to
// take, given the current conditions of the queue.
func (jq *jobHasSectorQueue) expectedJobTime() JobTime {
	return jq.staticDT.PercentilesCustom(jobTimePercentiles)[0]
}

// initJobHasSectorQueue will init the queue for the has sector jobs.
func (w *worker) initJobHasSectorQueue() {
	// Sanity check that there is no existing job queue.
	if w.staticJobHasSectorQueue != nil {
		w.staticRenter.staticLog.Critical("incorret call on initJobHasSectorQueue")
		return
	}

	w.staticJobHasSectorQueue = &jobHasSectorQueue{
		staticDT:        skymodules.NewDistributionTrackerStandard(),
		jobGenericQueue: newJobGenericQueue(w),
	}
}

// managedCallPostExecutionHook calls a post execution hook if registered. The
// hook will only be called the first time this method is executed. Subsequent
// calls are no-ops.
func (j *jobHasSector) managedCallPostExecutionHook(resp *jobHasSectorResponse) {
	if j.staticPostExecutionHook == nil {
		return // nothing to do
	}
	j.once.Do(func() {
		j.staticPostExecutionHook(resp)
	})
}

// hasSectorJobExpectedBandwidth is a helper function that returns the expected
// bandwidth consumption of a has sector job. This helper function enables
// getting at the expected bandwidth without having to instantiate a job.
func hasSectorJobExpectedBandwidth(numRoots int) (ul, dl uint64) {
	// closestMultipleOf is a small helper function that essentially rounds up
	// 'num' to the closest multiple of 'multipleOf'.
	closestMultipleOf := func(num, multipleOf int) int {
		mod := num % multipleOf
		if mod != 0 {
			num += (multipleOf - mod)
		}
		return num
	}

	// A HS job consumes more than one packet on download as soon as it contains
	// 13 roots or more. In terms of upload bandwidth that threshold is at 17.
	// To be conservative we use 10 and 15 as cutoff points.
	downloadMultiplier := closestMultipleOf(numRoots, 10) / 10
	uploadMultiplier := closestMultipleOf(numRoots, 15) / 15

	// A base of 1500 is used for the packet size. On ipv4, it is technically
	// smaller, but siamux is general and the packet size is the Ethernet MTU
	// (1500 bytes) minus any protocol overheads. It's possible if the renter is
	// connected directly over an interface to a host that there is no overhead,
	// which means siamux could use the full 1500 bytes. So we use the most
	// conservative value here as well.
	ul = uint64(1500 * uploadMultiplier)
	dl = uint64(1500 * downloadMultiplier)
	return
}
