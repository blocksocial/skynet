package renter

// TODO: Need have price per ms per worker set somewhere by the user, with some
// sane default.

// TODO: Better handling of the time.After calls.

// TODO: Refine the pricePerMS mechanic so that it's considering the cost of a
// whole. Right now it looks at each worker separately, which means that it may
// pay money to bump up the speed of a worker to faster worker, where both
// workers are already faster than the slowest worker available. Needs more
// thought. In the meantime, we're probably spending more money than we need to
// for speed.

import (
	"bytes"
	"context"
	"math"
	"time"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/types"

	"gitlab.com/NebulousLabs/errors"
)

// workerRank is a system for ranking workers based on how well the owrker has
// been performing and what pieces of a download a worker is capable of
// fetching.
type workerRank uint64

var (
	// workerRankErr implies that the workerRank value was initialized
	// incorrectly.
	workerRankErr = 0

	// workerRankUnlaunchedPiece means that the worker is in good standing and
	// can fetch a piece for which no other worker has launched.
	workerRankUnlaunchedPiece = 1

	// workerRankLaunchedPiece means that the worker is in good standing and can
	// fetch a piece that another worker launched to fetch. That other worker
	// however is not in good standing.
	workerRankLaunchedPiece = 2

	// workerRankActivePiece means that the worker is in good standing and can
	// fetch a piece that another worker launched to fetch. That other worker is
	// still in good standing.
	workerRankActivePiece = 3

	// workerRankLate means that the worker was launched to fetch another piece,
	// and the worker is late in returning that piece.
	workerRankLate = 4
)

// pieceDownload tracks a worker downloading a piece, whether that piece has
// returned, and what time the piece is/was expected to return.
//
// NOTE: The actual piece data is stored in the projectDownloadChunk after the
// download completes.
type pieceDownload struct {
	// 'completed', 'launched', and 'failed' are status variables for the piece.
	// If 'launched' is false, it means the piece download has not started yet.
	// Both 'completed' and 'failed' will also be false.
	//
	// If 'launched' is true and neither 'completed' nor 'failed' is true, it
	// means the download is in progress and the result is not known.
	//
	// Only one of 'completed' and 'failed' can be set to true. 'completed'
	// means the download was successful and the piece data was added to the
	// projectDownloadChunk. 'failed' means the download was unsuccessful.
	completed bool
	failed    bool
	launched  bool

	// expectedCompletionTime indicates the time when the download is expected
	// to complete. This is used to determine whether or not a download is late.
	expectedCompletionTime time.Time

	worker *worker
}

// projectDownloadChunk is a bunch of state that helps to orchestrate a download
// from a projectChunkWorkerSet.
//
// The projectDownloadChunk is only ever accessed by a single thread which
// orchestrates the download, which means that it does not need to be thread
// safe.
type projectDownloadChunk struct {
	// Parameters for downloading within the chunk.
	chunkLength uint64
	chunkOffset uint64
	pricePerMS  types.Currency

	// Values derived from the chunk download parameters. The offset and length
	// specify the offset and length that will be sent to the host, which much
	// be segment aligned.
	pieceLength uint64
	pieceOffset uint64

	// availablePieces are pieces where there are one or more workers that have
	// been tasked with fetching the piece.
	//
	// workersConsidered is a map of which workers have been moved from the
	// worker set's list of available pieces to the download chunk's list of
	// available pieces. This enables the worker selection code to realize which
	// pieces in the worker set have been resolved since the last check.
	availablePieces   [][]pieceDownload
	workersConsidered map[string]struct{}

	// dataPieces is the buffer that is used to place data as it comes back.
	// There is one piece per chunk, and pieces can be nil. To know if the
	// download is complete, the number of non-nil pieces will be counted.
	dataPieces [][]byte

	// The completed data gets sent down the response chan once the full
	// download is done.
	ctx                  context.Context
	downloadResponseChan chan *downloadResponse
	workerResponseChan   chan *jobReadResponse
	workerSet            *projectChunkWorkerSet
}

// downloadResponse is sent via a channel to the caller of
// 'projectChunkWorkerSet.managedDownload'.
type downloadResponse struct {
	data []byte
	err  error
}

// findBestWorker will look at all of the workers that can help fetch a new
// piece. If a good worker is found, that worker is returned along with the
// index of the piece it should fetch. If no good worker is found but a good
// worker is in the queue, two channels will be returned, either of which can
// signal that a new worker may be available.
//
// An error will only be returned if there are not enough workers to complete
// the download. If there are enough workers to complete the download, but all
// potential workers have already been launched, all of the return values will
// be nil.
//
// If there are no workers at all that may be able to contribute, an error is
// returned, which means the download must either proceed with its current set
// of workers or must fail.
//
// NOTE: There is an edge case where all of the pieces are already active. In
// this case, a worker may be returned that overlaps with another worker. From
// an erasure coding perspective, this is inefficient, but can be useful if
// there is an expectation that lots of existing workers will fail.
//
// TODO: Need to migrate over any unresolved workers.
func (pdc *projectDownloadChunk) findBestWorker() (*worker, uint64, time.Duration, <-chan struct{}, error) {
	// Helper variables.
	//
	// NOTE: pricePerMSPerWorker is actually going to be an over-estimate in
	// most cases. In the worst case, a renter is going to need to pay every
	// worker to speed up, but in a typical case going X milliseconds faster
	// only requires cycling out 1-2 slow workers rather than completely
	// choosing a different set of workers. The code to make this more accurate
	// in the typical case is more complex than is worth implementing at this
	// time.
	ws := pdc.workerSet
	pricePerMSPerWorker := pdc.pricePerMS.Mul64(uint64(ws.staticErasureCoder.MinPieces()))

	// For lock safety, need to fetch the list of unresolved workers separately
	// from learning the best time. For thread safety, the update channel needs
	// to be created at the moment that we observe the set of unresovled
	// workers.
	var unresolvedWorkers []*pcwsUnresolvedWorker
	ws.mu.Lock()
	for _, uw := range ws.unresolvedWorkers {
		unresolvedWorkers = append(unresolvedWorkers, uw)
	}
	ws.mu.Unlock()

	// Find the best duration of any unresolved worker. This will be compared to
	// the best duration of any resolved worker which could be put to work on a
	// piece.
	//
	// bestUnresolvedWaitTime is the amount of time that the renter should wait
	// to expect to hear back from the worker.
	nullDuration := time.Duration(math.MaxInt64)
	bestUnresolvedDuration := nullDuration
	bestWorkerLate := true
	var bestUnresolvedWaitTime time.Duration
	for _, uw := range unresolvedWorkers {
		// Figure how much time is expected to remain until the worker is
		// avaialble. Note that no price penatly is attached to the HasSector
		// call, because that call is being made regardless of the cost.
		hasSectorTime := time.Until(uw.staticExpectedCompletionTime)
		if hasSectorTime < 0 {
			hasSectorTime = 0
		}

		// Figure out how much time is expected until the worker completes the
		// download job.
		readTime := uw.staticWorker.staticJobReadQueue.callExpectedJobTime(pdc.pieceLength)
		if readTime < 0 {
			readTime = 0
		}
		// Add a penalty to performance based on the cost of the job. Need to be
		// careful with the underflow cases.
		expectedRSCompletionCost := uw.staticWorker.staticJobReadQueue.callExpectedJobCost(pdc.pieceLength)
		rsPricePenalty, err := expectedRSCompletionCost.Div(pricePerMSPerWorker).Uint64()
		if err != nil || rsPricePenalty > math.MaxInt64 {
			readTime = time.Duration(math.MaxInt64)
		} else if reduced := math.MaxInt64 - int64(rsPricePenalty); int64(readTime) > reduced {
			readTime = time.Duration(math.MaxInt64)
		} else {
			readTime += time.Duration(rsPricePenalty)
		}

		// Compare the total time (including price preference) to the current
		// best time. Workers that are not late get preference over workers that
		// are late.
		adjustedTotalDuration := hasSectorTime + readTime
		betterLateStatus := bestWorkerLate && hasSectorTime > 0
		betterDuration := adjustedTotalDuration < bestUnresolvedDuration
		if betterLateStatus || betterDuration {
			bestUnresolvedDuration = adjustedTotalDuration
		}
		if hasSectorTime > 0 && betterDuration {
			// bestUnresolvedWaitTime should be set to 0 unless the best
			// unresolved worker is not late.
			bestUnresolvedWaitTime = hasSectorTime
		}
		// The first time we find a worker that is not late, the best worker is
		// not late. Marking this several times is not an issue.
		if hasSectorTime > 0 {
			bestWorkerLate = false
		}
	}

	// Copy the list of resolved workers.
	now := time.Now() // So we aren't calling time.Now() a ton. It's a little expensive.
	// Count how many pieces could be fetched where no unfailed workers have
	// been launched for the piece yet.
	piecesAvailableToLaunch := 0
	// Track whether there are any unlaunched workers at all. If this is false
	// and there are also no unresolved workers, then no additional workers can
	// be launched at all, and the find workers function should terminate.
	unlaunchedWorkersAvailable := false
	// Track whether there are any pieces that don't have a worker launched
	// which hasn't failed. This is to determine the rank of any unresolved
	// workers. If there are unlaunched pieces, the rank of any unresolved
	// workers is 'workerRankUnlaunchedPiece'.
	unlaunchedPieces := false
	// Track whether there are any pieces where there are workers that have
	// launched and not failed, but all workers that have launched and not
	// failed are late. This is to determine the rank of any unresovled workers.
	// If there are inactive pieces and no unlaunched pieces, the rank of any
	// unresolved workers is 'workerRankLaunchedPiece'. If there are no inactive
	// pieces and also no unlaunched pieces, the rank of any unresolved workers
	// is 'workerRankActivePiece'.
	inactivePieces := false
	// Save a list of which workers are currently late.
	lateWorkers := make(map[string]struct{})
	ws.mu.Lock()
	piecesCopy := make([][]pieceDownload, len(pdc.availablePieces))
	for i, activePiece := range pdc.availablePieces {
		// Track whether there are new workers available for this piece.
		unlaunchedWorkerAvailable := false
		// Track whether any of the workers have launched a job and have not yet
		// failed.
		launchedWithoutFail := false
		// Track whether the piece has no launched workers at all.
		unlaunchedPiece := true
		// Track whether the piece has any late workers.
		pieceHasLateWorkers := false
		// Track whether the piece has any workers that are not yet late.
		pieceHasActiveWorkers := false
		piecesCopy[i] = make([]pieceDownload, len(activePiece))
		for j, pieceDownload := range activePiece {
			// Consistency check - failed and completed are mutally exclusive,
			// and neither should be set unless launched is set.
			if (!pieceDownload.launched && (pieceDownload.completed || pieceDownload.failed)) || (pieceDownload.failed && pieceDownload.completed) {
				ws.staticRenter.log.Critical("rph3 download piece is incoherent")
			}
			piecesCopy[i][j] = pieceDownload

			// If this worker has not launched, there are workers that can fetch
			// this piece. That also means there are more workers that can be
			// launched if this download is struggling.
			if !pieceDownload.launched {
				unlaunchedWorkerAvailable = true
				unlaunchedWorkersAvailable = true
			}
			// If this worker has launched and not yet failed, this piece is not
			// an unlaunched piece.
			if pieceDownload.launched && !pieceDownload.failed {
				unlaunchedPiece = false
			}
			// If there is a worker that has launched and not yet failed, this
			// piece can be counted as a piece which has launched without fail -
			// it is a piece that may contribute to redundancy in the future.
			if pieceDownload.launched && !pieceDownload.failed {
				launchedWithoutFail = true
			}
			// Check if this piece is late or if the piece failed altogether, if
			// so, mark the worker as a late worker.
			if pieceDownload.launched && (pieceDownload.failed || pieceDownload.expectedCompletionTime.Before(now)) {
				lateWorkers[pieceDownload.worker.staticHostPubKeyStr] = struct{}{}
				pieceHasLateWorkers = true
			} else if pieceDownload.launched {
				pieceHasActiveWorkers = true
			}
		}
		// Count the piece as able to contribute to redundancy if there is a
		// worker that has launched for the piece which has not failed.
		if launchedWithoutFail || unlaunchedWorkerAvailable {
			piecesAvailableToLaunch++
		}
		// If this piece does not have any workers that have launched without
		// fail, this piece counts as an unlaunched piece.
		if unlaunchedPiece {
			unlaunchedPieces = true
		}
		// If this piece has late workers, and also has no workers that are
		// launched and not yet late, then this piece counts as inactive.
		if pieceHasLateWorkers && !pieceHasActiveWorkers {
			inactivePieces = true
		}
	}
	ws.mu.Unlock()

	// Check whether it is still possible for the download to complete.
	potentialPieces := piecesAvailableToLaunch + len(unresolvedWorkers)
	if potentialPieces < ws.staticErasureCoder.MinPieces() {
		return nil, 0, 0, nil, errors.New("rph3 chunk download has failed because there are not enough potential workers")
	}
	// Check whether it is possible for new workers to be launched.
	if !unlaunchedWorkersAvailable && len(unresolvedWorkers) == 0 {
		// All 'nil' return values, meaning the download can succeed by waiting
		// for already launched workers to return, but cannot succeed by
		// launching new workers because no new workers are available.
		return nil, 0, 0, nil, nil
	}

	// We know from the previous check that at least one of the unresolved
	// workers or at least one of the resolved workers is available. Initialize
	// the variables as though there are no unresolved workers, and then if
	// there is an unresolved worker, set the rank and duration appropriately.
	//
	// TODO: We have the worker selection process complete, but we don't
	// actually have the code to finish the return values. Also, we should
	// probably return a time.Time instead of a time channel, because not every
	// worker that gets returned is a blocking element.
	bestWorkerResolved := true
	bestKnownRank := workerRankLate
	bestKnownDuration := nullDuration
	if len(unresolvedWorkers) > 0 {
		// Determine the rank of the unresolved workers. We rank unresolved
		// workers optimistically, meaning we assume that they will fill the
		// most convenient / important possible role.
		if unlaunchedPieces {
			// If there are any pieces that have not yet launched workers, then
			// the rank of any unresolved workers is going to be 'unlaunched'.
			bestKnownRank = workerRankUnlaunchedPiece
		} else if inactivePieces {
			// If there are no unlaunched pieces, but there are inactive pieces,
			// the rank of any unresolved workers is going to be 'launched'.
			bestKnownRank = workerRankLaunchedPiece
		} else {
			// If there are no unlaunched pieces and no inactive pieces, the
			// rank of any unlaunched workers is going to be 'active'.
			bestKnownRank = workerRankActivePiece
		}
		bestWorkerResolved = false
		bestKnownDuration = bestUnresolvedDuration
	}
	// Iterate through the piecesCopy, finding the best unlaunched worker in the
	// set. Only count workers that are better than the best unresolved worker.
	var bestResolvedWorker *worker
	var bestPieceIndex uint64
	for _, activePiece := range piecesCopy {
		pieceCompleted := false
		pieceActive := false
		pieceLaunched := false
		for _, pieceDownload := range activePiece {
			if pieceCompleted {
				pieceCompleted = true
				break
			}
			if pieceDownload.launched {
				pieceLaunched = true
			}
			if pieceDownload.launched && now.Before(pieceDownload.expectedCompletionTime) {
				pieceActive = true
			}
		}
		// Skip this piece if the piece has already been completed.
		if pieceCompleted {
			continue
		}
		// Skip this piece if it the rank of the piece is worse than the best
		// known rank.
		if bestKnownRank < workerRankLaunchedPiece && pieceLaunched {
			continue
		}
		// Skip this piece if it the rank of the piece is worse than the best
		// known rank.
		if bestKnownRank < workerRankActivePiece && pieceActive {
			continue
		}

		// Look for any workers of good enough rank.
		for i, pieceDownload := range activePiece {
			// Skip any workers that are late if the best known rank is not
			// late.
			_, isLate := lateWorkers[pieceDownload.worker.staticHostPubKeyStr]
			if bestKnownRank < workerRankLate && isLate {
				continue
			}
			// Skip this worker if it is not good enough.
			w := pieceDownload.worker
			readTime := w.staticJobReadQueue.callExpectedJobTime(pdc.pieceLength)
			if readTime < 0 {
				readTime = 0
			}
			expectedRSCompletionCost := w.staticJobReadQueue.callExpectedJobCost(pdc.pieceLength)
			rsPricePenalty, err := expectedRSCompletionCost.Div(pricePerMSPerWorker).Uint64()
			if err != nil || rsPricePenalty > math.MaxInt64 {
				readTime = time.Duration(math.MaxInt64)
			} else if reduced := math.MaxInt64 - int64(rsPricePenalty); int64(readTime) > reduced {
				readTime = time.Duration(math.MaxInt64)
			} else {
				readTime += time.Duration(rsPricePenalty)
			}
			if bestKnownDuration < readTime {
				continue
			}

			// This worker is good enough, determine the new rank.
			if isLate {
				bestKnownRank = workerRankLate
			} else if pieceActive {
				bestKnownRank = workerRankActivePiece
			} else if pieceLaunched {
				bestKnownRank = workerRankLaunchedPiece
			} else {
				bestKnownRank = workerRankUnlaunchedPiece
			}
			bestKnownDuration = readTime
			bestResolvedWorker = pieceDownload.worker
			bestPieceIndex = uint64(i)
			bestWorkerResolved = true
		}
	}

	// If the best worker is an unresolved worker, return a time that indicates
	// when we should give up waiting for the worker, as well as a channel that
	// indicates when there are new workers that have returned.
	if !bestWorkerResolved {
		ws.mu.Lock()
		c := ws.registerForWorkerUpdate()
		ws.mu.Unlock()
		checkAgainTime := bestUnresolvedWaitTime
		if bestWorkerLate {
			checkAgainTime = 0
		}
		return nil, 0, checkAgainTime, c, nil
	}

	// Best worker is resolved.
	if bestResolvedWorker == nil {
		ws.staticRenter.log.Critical("there is no best resolved worker and also no best unresolved worker")
	}
	return bestResolvedWorker, bestPieceIndex, 0, nil, nil
}

// waitForWorker will block until one of the best workers is available to be
// used for download, along with the index of the piece that should be
// downloaded. An error will be returned if there are no new workers available
// that can be launched.
//
// TODO: Need thorough testing to ensure that repeated calls to findBestWorker
// eventually fail. I guess the context timeout actually handles most of this.
//
// TODO: Do something about that time.After
func (pdc *projectDownloadChunk) waitForWorker() (*worker, uint64, error) {
	for {
		worker, pieceIndex, sleepTime, wakeChan, err := pdc.findBestWorker()
		if err != nil {
			return nil, 0, errors.AddContext(err, "no good worker could be found")
		}
		// If there was a worker found, return that worker.
		if worker != nil {
			return worker, pieceIndex, nil
		}

		// If there was no worker found, sleep until we should call
		// findBestWorker again.
		maxSleep := time.After(sleepTime)
		select {
		case <-maxSleep:
		case <-wakeChan:
		case <-pdc.ctx.Done():
			return nil, 0, errors.New("timed out waiting for a good worker")
		}
	}
}

// launchWorker will launch a worker for the download project. An error
// will be returned if there is no worker to launch.
func (pdc *projectDownloadChunk) launchWorker() error {
	// Loop until either a worker succeeds in launching a job, or until there
	// are no more workers to return. The exit condition for this loop depends
	// on waitForWorker() being guaranteed to return an error if the workers
	// keep failing. When a worker fails, we set 'pieceDownload.failed' to true,
	// which causes waitForWorker to ignore that worker option in the future.
	// There are a finite number of worker options total.
	for {
		// An error here means that no more workers are available at all.
		w, pieceIndex, err := pdc.waitForWorker()
		if err != nil {
			return errors.AddContext(err, "unable to launch a new worker")
		}

		// Create the read sector job for the worker.
		jrs := &jobReadSector{
			jobRead: jobRead{
				staticResponseChan: pdc.workerResponseChan,
				staticLength:       pdc.pieceLength,

				staticSector: pdc.workerSet.staticPieceRoots[pieceIndex],

				jobGeneric: newJobGeneric(w.staticJobReadQueue, pdc.ctx.Done()),
			},
			staticOffset: pdc.pieceOffset,
		}

		// Launch the job and then update the status of the worker. Either way,
		// the worker should be marked as 'launched'. If the job is not
		// successfully queued, the worker should be marked as 'failed' as well.
		//
		// NOTE: We don't break out of the loop when we find a piece/worker
		// match. If all is going well, each worker should appear at most once
		// in this piece, but for the sake of defensive programming we check all
		// elements anyway.
		expectedCompletionTime, added := w.staticJobReadQueue.callAddWithEstimate(jrs)
		for _, pieceDownload := range pdc.availablePieces[pieceIndex] {
			if w.staticHostPubKeyStr == pieceDownload.worker.staticHostPubKeyStr {
				pieceDownload.launched = true
				if added {
					pieceDownload.expectedCompletionTime = expectedCompletionTime
				} else {
					pieceDownload.failed = true
				}
			}
		}

		// If there was no error, return the expected completion time.
		// Otherwise, try grabbing a new worker.
		if added {
			return nil
		}
	}
}

// handleJobReadResponse will take a jobReadResponse from a worker job
// and integrate it into the set of pieces.
func (pdc *projectDownloadChunk) handleJobReadResponse(jrr *jobReadResponse) {
	// Prevent a production panic.
	if jrr == nil {
		pdc.workerSet.staticRenter.log.Critical("received nil job read response in handleJobReadResponse")
		return
	}

	// Figure out which index this read corresponds to.
	pieceIndex := 0
	for i, root := range pdc.workerSet.staticPieceRoots {
		if jrr.staticSectorRoot == root {
			pieceIndex = i
			break
		}
	}

	// Check whether the job failed.
	if jrr.staticErr != nil {
		// TODO: Log? - we should probably have toggle-able log levels for stuff
		// like this. Maybe a worker.log which allows us to turn on logging just
		// for specific workers.
		//
		// The download failed, update the pdc available pieces to reflect the
		// failure.
		for i := 0; i < len(pdc.availablePieces[pieceIndex]); i++ {
			if pdc.availablePieces[pieceIndex][i].worker.staticHostPubKeyStr == jrr.staticWorker.staticHostPubKeyStr {
				pdc.availablePieces[pieceIndex][i].failed = true
			}
		}
		return
	}

	// The download succeeded, add the piece to the appropriate index.
	pdc.dataPieces[pieceIndex] = jrr.staticData
	jrr.staticData = nil // Just in case there's a reference to the job reponse elsewhere.
	for i := 0; i < len(pdc.availablePieces[pieceIndex]); i++ {
		if pdc.availablePieces[pieceIndex][i].worker.staticHostPubKeyStr == jrr.staticWorker.staticHostPubKeyStr {
			pdc.availablePieces[pieceIndex][i].completed = true
		}
	}
}

// fail will send an error down the download response channel.
func (pdc *projectDownloadChunk) fail(err error) {
	dr := &downloadResponse{
		data:  nil,
		err: err,
	}
	pdc.downloadResponseChan <- dr
}

// finalize will take the completed pieces of the download, decode them,
// and then send the result down the response channel. If there is an error
// during decode, 'pdc.fail()' will be called.
func (pdc *projectDownloadChunk) finalize() {
	// Helper variable.
	ec := pdc.workerSet.staticErasureCoder

	// The chunk download offset and chunk download length are different from
	// the requested offset and length because the chunk download offset and
	// length are required to be
	chunkDLOffset := pdc.pieceOffset * uint64(ec.MinPieces())
	chunkDLLength := pdc.pieceOffset * uint64(ec.MinPieces())

	buf := bytes.NewBuffer(nil)
	err := pdc.workerSet.staticErasureCoder.Recover(pdc.dataPieces, chunkDLOffset+chunkDLLength, buf)
	if err != nil {
		pdc.fail(errors.AddContext(err, "unable to complete erasure decode of download"))
		return
	}

	// Data is all in, truncate the chunk accordingly.
	//
	// TODO: Unit test this.
	data := buf.Bytes()
	chunkStartWithinData := pdc.chunkOffset - chunkDLOffset
	chunkEndWithinData := pdc.chunkLength + chunkStartWithinData
	data = data[chunkStartWithinData:chunkEndWithinData]

	// The data is all set.
	dr := &downloadResponse{
		data: data,
		err:  nil,
	}
	pdc.downloadResponseChan <- dr
}

// finished returns true if the download is finished, and returns an error if
// the download is unable to complete.
func (pdc *projectDownloadChunk) finished() (bool, error) {
	// Convenience variables.
	ws := pdc.workerSet
	ec := pdc.workerSet.staticErasureCoder

	// Count the number of completed pieces and hopefuly pieces in our list of
	// potential downloads.
	completedPieces := 0
	hopefulPieces := 0
	for _, piece := range pdc.availablePieces {
		// Only count one piece as hopeful per set.
		hopeful := false
		for _, pieceDownload := range piece {
			// If this piece is completed, count it both as hopeful and
			// completed, no need to look at other pieces.
			if pieceDownload.completed {
				hopeful = true
				completedPieces++
				break
			}
			// If this piece has not yet failed, it is hopeful. Keep looking
			// through the pieces in case there is a completed piece.
			if !pieceDownload.failed {
				hopeful = true
			}
		}
		if hopeful {
			hopefulPieces++
		}
	}
	if completedPieces >= ec.MinPieces() {
		return true, nil
	}

	// Count the number of workers that haven't completed their results yet.
	ws.mu.Lock()
	hopefulPieces += len(ws.unresolvedWorkers)
	ws.mu.Unlock()

	// Ensure that there are enough pieces that could potentially become
	// completed to finish the download.
	if hopefulPieces < ec.MinPieces() {
		return false, errors.New("not enough pieces to complete download")
	}
	return false, nil
}

// needsOverdrive returns true if the function determines that another piece
// should be launched to assist with the current download.
func (pdc *projectDownloadChunk) needsOverdrive() (time.Duration, bool) {
	// Go through the pieces, determining how many pieces are launched without
	// fail, and what the longest return time is of all the workers that have
	// already been launched.
	numLWF := 0
	var latestReturn time.Time
	for _, piece := range pdc.availablePieces {
		launchedWithoutFail := false
		for _, pieceDownload := range piece {
			if pieceDownload.launched && !pieceDownload.failed {
				launchedWithoutFail = true
				if !pieceDownload.completed && latestReturn.Before(pieceDownload.expectedCompletionTime) {
					latestReturn = pieceDownload.expectedCompletionTime
				}
			}
		}
		if launchedWithoutFail {
			numLWF++
		}
	}

	// If the number of pieces that have launched without fail is less than the
	// minimum number of pieces need to complete a download, overdrive is
	// required.
	if numLWF < pdc.workerSet.staticErasureCoder.MinPieces() {
		// No need to return a time, need to launch a worker immediately.
		return 0, true
	}

	// If the latest worker should have already returned, signal than an
	// overdrive worker should be launched immediately.
	untilLatest := time.Until(latestReturn)
	if untilLatest <= 0 {
		return 0, true
	}

	// There are enough workers out, and it is expected that not all of them
	// have returned yet. Signal that we should check again 50 milliseconds
	// after the latest worker has failed to return.
	//
	// Note that doing things this way means that launching new workers will
	// cause the latest time returned to reflect their latest time - each time
	// an overdrive worker is launched, we will wait the full return period
	// before launching another one.
	return (untilLatest + time.Millisecond * 50), false
}

// threadedCollectAndOverdrivePieces is the maintenance function of the download
// process.
//
// NOTE: One potential optimization here is to pre-emptively launch a few
// overdrive pieces, in a similar fashion that the other code does. We can do it
// more intelligently here though, tracking over time what percentage of
// downloads comlete without failure, and what percentage of downloads end up
// needing overdrive pieces. Then we can use that failure rate to determine how
// often we should pre-emtively launch some overdrive pieces rather than wait
// for the failures to happen. This many not be necessary at all if the failure
// rates are low enough.
func (pdc *projectDownloadChunk) threadedCollectAndOverdrivePieces() {
	// Loop until the download has either failed or completed.
	for {
		// Check whether the download is comlete. An error means that the
		// download has failed and can no longer make progress.
		completed, err := pdc.finished()
		if completed {
			pdc.finalize()
			return
		}
		if err != nil {
			pdc.fail(err)
			return
		}

		// Drain the response chan of any results that have been submitted.
		select {
		case jrr := <-pdc.workerResponseChan:
			pdc.handleJobReadResponse(jrr)
			continue
		case <-pdc.ctx.Done():
			pdc.fail(errors.New("download failed while waiting for responses"))
			return
		}

		// Run logic to determine whether or not we should kick off an overdrive
		// worker. We skip checking the error on the launch because
		// 'pdc.finished()' will catch the error on the next iteration of the
		// outer loop.
		overdriveTimeout, needsOverdrive := pdc.needsOverdrive()
		if needsOverdrive {
			_ = pdc.launchWorker() // Err is ignored, nothing to do.
			continue
		}

		// Determine when the next overdrive check needs to run.
		//
		// TODO: Should be able to create a cache for the timer in the pdc,
		// which hopefully allows us to avoid the memory allocation cost
		// associated with calling time.After() a bunch of times.
		overdriveTimeoutChan := time.After(overdriveTimeout)
		select {
		case jrr := <-pdc.workerResponseChan:
			pdc.handleJobReadResponse(jrr)
			continue
		case <-pdc.ctx.Done():
			pdc.fail(errors.New("download failed while waiting for responses"))
			return
		case <-overdriveTimeoutChan:
			continue
		}
	}
}

// getPieceOffsetAndLen is a helper function to compute the piece offset and
// length of a chunk download, given the erasure coder for the chunk, the offset
// within the chunk, and the length within the chunk.
func getPieceOffsetAndLen(ec modules.ErasureCoder, offset, length uint64) (pieceOffset, pieceLength uint64) {
	// Fetch the segment size of the ec.
	pieceSegmentSize, partialsSupported := ec.SupportsPartialEncoding()
	if !partialsSupported {
		// If partials are not supported, the full piece needs to be downloaded.
		pieceSegmentSize = modules.SectorSize
	}

	// Consistency check some of the erasure coder values. If the check fails,
	// return that the whole piece must be downloaded.
	if pieceSegmentSize == 0 || pieceSegmentSize%crypto.SegmentSize != 0 {
		build.Critical("pcws has a bad erasure coder")
		return 0, modules.SectorSize
	}

	// Determine the download offset within a single piece. We get this by
	// dividing the chunk offset by the number of pieces and then rounding
	// down to the nearest segment size.
	//
	// This is mathematically equivalent to rounding down the chunk size to
	// the nearest chunk segment size and then dividing by the number of
	// pieces.
	pieceOffset = offset / uint64(ec.MinPieces())
	pieceOffset = pieceOffset / pieceSegmentSize
	pieceOffset = pieceOffset * pieceSegmentSize

	// Determine the length that needs to be downloaded. This is done by
	// determining the offset that the download needs to reach, and then
	// subtracting the pieceOffset from the termination offset.
	chunkSegmentSize := pieceSegmentSize * uint64(ec.MinPieces())
	chunkTerminationOffset := offset + length
	overflow := chunkTerminationOffset % chunkSegmentSize
	if overflow != 0 {
		chunkTerminationOffset += chunkSegmentSize - overflow
	}
	pieceTerminationOffset := chunkTerminationOffset / pieceSegmentSize
	pieceLength = pieceTerminationOffset - pieceOffset
	return pieceOffset, pieceLength
}

// 'managedDownload' will download a range from a chunk. This call is
// asynchronous. It will return as soon as the sector download requests have
// been sent to the workers. This means that it will block until enough workers
// have reported back with HasSector results that the optimal download request
// can be made. Where possible, the projectChunkWorkerSet should be created in
// advance of the download call, so that the HasSector calls have as long as
// possible to complete, cutting significant latency off of the download.
//
// Blocking until all of the piece downloads have been put into job queues
// ensures that the workers will account for the bandwidth overheads associated
// with these jobs before new downloads are requested. Multiple calls to
// 'managedDownload' from the same thread will generally follow the rule that
// the first calls will return first. This rule cannot be enforced if the call
// to managedDownload returns before the download jobs are queued into the
// workers.
//
// pricePerMS is "price per millisecond". To add a price preference to picking
// hosts to download from, the total cost of performing a download will be
// converted into a number of milliseconds based on the pricePerMS. This cost
// will be added to the return time of the host, meaning the host will be
// selected as though it is slower.
//
// TODO: If an error is returned, the input contex should probably be closed. An
// alternative context design here would be to create a child context, and then
// close the child context if the launch is not successful, but then we would
// not be closing the child context at all in the success case, which is
// considered a context anti-pattern. I'm not sure if this design should be kept
// or if something else is preferred.
//
// NOTE: A lot of this code is not future proof against certain classes of
// encryption algorithms and erasure coding algorithms, however I believe that
// the properties we have in our current set (maximum distance separable erasure
// codes, tweakable encryption algorithms) are not likely to change in the
// future.
func (pcws *projectChunkWorkerSet) managedDownload(ctx context.Context, pricePerMS types.Currency, offset, length uint64) (chan *downloadResponse, error) {
	// Convenience variables.
	ec := pcws.staticErasureCoder

	// Check encryption type. If the encryption overhead is not zero, the piece
	// offset and length need to download the full chunk. This is due to the
	// overhead being a checksum that has to be verified against the entire
	// piece.
	//
	// NOTE: These checks assume that any upload with encryption overhead needs
	// to be downloaded as full sectors. This feels reasonable because smaller
	// sectors were not supported when encryption schemes with overhead were
	// being suggested.
	if pcws.staticMasterKey.Type().Overhead() != 0 && (offset != 0 || length != modules.SectorSize*uint64(ec.MinPieces())) {
		return nil, errors.New("invalid request performed - this chunk has encryption overhead and therefore the full chunk must be downloaded")
	}

	// Determine the offset and length that needs to be downloaded from the
	// pieces. This is non-trivial because both the network itself and also the
	// erasure coder have required segment sizes.
	pieceOffset, pieceLength := getPieceOffsetAndLen(ec, offset, length)

	// Create the workerResponseChan.
	//
	// The worker response chan is allocated to be quite large. This is because
	// in the worst case, the total number of jobs submitted will be equal to
	// the number of workers multiplied by the number of pieces. We do not want
	// workers blocking when they are trying to send down the channel, so a very
	// large buffered channel is used. Each element in the channel is only 8
	// bytes (it is just a pointer), so allocating a large buffer doesn't
	// actually have too much overhead. Instead of buffering for a full
	// workers*pieces slots, we buffer for pieces*5 slots, under the assumption
	// that the overdrive code is not going to be so aggressive that 5x or more
	// overhead on download will be needed.
	//
	// TODO: If this ends up being a problem, we could implement the jobs
	// process to send the result down a channel in goroutine if the first
	// attempt to send the job fails. Then we could probably get away with a
	// smaller buffer, since exceeding the limit currently would cause a worker
	// to stall, where as with the goroutine-on-block method, exceeding the
	// limit merely causes extra goroutines to be spawned.
	workerResponseChan := make(chan *jobReadResponse, ec.NumPieces()*5)

	// Build the full pdc.
	pdc := &projectDownloadChunk{
		chunkOffset: offset,
		chunkLength: length,
		pricePerMS:  pricePerMS,

		pieceOffset: pieceOffset,
		pieceLength: pieceLength,

		availablePieces:   make([][]pieceDownload, ec.NumPieces()),
		workersConsidered: make(map[string]struct{}),

		dataPieces: make([][]byte, ec.NumPieces()),

		ctx:                  ctx,
		workerResponseChan:   workerResponseChan,
		downloadResponseChan: make(chan *downloadResponse, 1),
		workerSet:            pcws,
	}

	// Launch enough workers to complete the download. The overdrive code will
	// determine whether more pieces need to be launched.
	for i := 0; i < pcws.staticErasureCoder.MinPieces(); i++ {
		// Try launching a worker. If the launch fails, it means that no workers
		// can be launched, and therefore the download cannot complete.
		err := pdc.launchWorker()
		if err != nil {
			return nil, errors.AddContext(err, "not enough workers to kick off the download")
		}
	}

	// All initial workers have been launched. The function can return now,
	// unblocking the caller. A background thread will be launched to collect
	// the reponses and launch overdrive workers when necessary.
	go pdc.threadedCollectAndOverdrivePieces()
	return pdc.downloadResponseChan, nil
}
