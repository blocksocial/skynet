package renter

type (
	// worstIgnoredHealth is a helper struct for the callBuildAndPushChunks
	// function. The struct and methods on the data are split out to improve
	// ease of testing.
	worstIgnoredHealth struct {
		health float64
		remote bool

		nextDirHealth float64
		nextDirRemote bool

		target repairTarget
	}
)

// updateWorstIgnoredHealth takes the health of a chunk that is being skipped
// and updates the worst known health to account for this chunk.
func (wih *worstIgnoredHealth) updateWorstIgnoredHealth(newHealth float64, newHealthRemote bool) {
	// The new health is not worse if it is not remote, but the worst health is
	// remote.
	if !newHealthRemote && wih.remote {
		return
	}
	// The new health is worse if it is remote and the current health is not
	// remote.
	if newHealthRemote && !wih.remote {
		wih.health = newHealth
		wih.remote = newHealthRemote
		return
	}
	// The remote values match for the new health and the current health. Update
	// the current health only if the new health is worse.
	if wih.health < newHealth {
		wih.health = newHealth
		return
	}
}

// canSkip contains the logic for determining whether a chunk can be skipped
// based on the worst health of any chunk so far skipped, and also based on the
// worst health of any chunk in the next directory in the directory heap.
//
// We want to make sure that the upload heap has all of the absolute worst
// chunks in the renter in it, so we want to skip any chunk that we know is in
// better health than any chunk which is being ignored because its in another
// directory.
func (wih *worstIgnoredHealth) canSkip(chunkHealth float64, chunkRemote bool) bool {
	// Cannot skip any chunks if we are not targeting unstuck chunks.
	if wih.target != targetUnstuckChunks {
		return false
	}

	// If this chunk is not remote and there are skipped chunks that are
	// remote, this chunk can be skipped.
	if !chunkRemote && (wih.remote || wih.nextDirRemote) {
		return true
	}
	// If this chunk is remote and nothing that has been skipped is remote,
	// this chunk cannot be skipped.
	if chunkRemote && !wih.remote && !wih.nextDirRemote {
		return false
	}
	// If the chunk is not remote, neither are either of the other values (those
	// possibilities checked above).
	//
	// This chunk can be skipped only if its health is better than those
	// other chunks.
	if !chunkRemote && (chunkHealth < wih.nextDirHealth || chunkHealth < wih.health) {
		return true
	} else if !chunkRemote {
		return false
	}

	// By elimination, the chunk is remote, and at least one of the other values
	// (maybe both) is remote. Grab the worst health of the two, and compare the
	// chunk health to that.
	var reqHealth float64
	if wih.nextDirRemote {
		reqHealth = wih.nextDirHealth
	}
	if wih.remote && reqHealth < wih.health {
		reqHealth = wih.health
	}
	return chunkHealth < reqHealth
}
