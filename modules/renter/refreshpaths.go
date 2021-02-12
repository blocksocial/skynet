package renter

import (
	"fmt"
	"sync"

	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/errors"
)

// uniqueRefreshPaths is a helper struct for determining the minimum number of
// directories that will need to have callThreadedBubbleMetadata called on in
// order to properly update the affected directory tree. Since bubble calls
// itself on the parent directory when it finishes with a directory, only a call
// to the lowest level child directory is needed to properly update the entire
// directory tree.
type uniqueRefreshPaths struct {
	childDirs  map[modules.SiaPath]struct{}
	parentDirs map[modules.SiaPath]struct{}

	r  *Renter
	mu sync.Mutex
}

// newUniqueRefreshPaths returns an initialized uniqueRefreshPaths struct
func (r *Renter) newUniqueRefreshPaths() *uniqueRefreshPaths {
	return &uniqueRefreshPaths{
		childDirs:  make(map[modules.SiaPath]struct{}),
		parentDirs: make(map[modules.SiaPath]struct{}),

		r: r,
	}
}

// callAdd adds a path to uniqueRefreshPaths.
func (urp *uniqueRefreshPaths) callAdd(path modules.SiaPath) error {
	urp.mu.Lock()
	defer urp.mu.Unlock()

	// Check if the path is in the parent directory map
	if _, ok := urp.parentDirs[path]; ok {
		return nil
	}

	// Check if the path is in the child directory map
	if _, ok := urp.childDirs[path]; ok {
		return nil
	}

	// Add path to the childDir map
	urp.childDirs[path] = struct{}{}

	// Check all path elements to make sure any parent directories are removed
	// from the child directory map and added to the parent directory map
	for !path.IsRoot() {
		// Get the parentDir of the path
		parentDir, err := path.Dir()
		if err != nil {
			contextStr := fmt.Sprintf("unable to get parent directory of %v", path)
			return errors.AddContext(err, contextStr)
		}
		// Check if the parentDir is in the childDirs map
		if _, ok := urp.childDirs[parentDir]; ok {
			// Remove from childDir map and add to parentDir map
			delete(urp.childDirs, parentDir)
		}
		// Make sure the parentDir is in the parentDirs map
		urp.parentDirs[parentDir] = struct{}{}
		// Set path equal to the parentDir
		path = parentDir
	}
	return nil
}

// callNumChildDirs returns the number of child directories currently being
// tracked.
func (urp *uniqueRefreshPaths) callNumChildDirs() int {
	urp.mu.Lock()
	defer urp.mu.Unlock()
	return len(urp.childDirs)
}

// callNumParentDirs returns the number of parent directories currently being
// tracked.
func (urp *uniqueRefreshPaths) callNumParentDirs() int {
	urp.mu.Lock()
	defer urp.mu.Unlock()
	return len(urp.parentDirs)
}

// callRefreshAll will updated the directories in the childDir map by calling
// refreshAll in a go routine.
func (urp *uniqueRefreshPaths) callRefreshAll() error {
	urp.mu.Lock()
	defer urp.mu.Unlock()
	return urp.r.tg.Launch(func() {
		err := urp.refreshAll()
		if err != nil {
			urp.r.log.Println("WARN: error with uniqueRefreshPaths refreshAll:", err)
		}
	})
}

// callRefreshAllBlocking will updated the directories in the childDir map by
// calling refreshAll.
func (urp *uniqueRefreshPaths) callRefreshAllBlocking() error {
	urp.mu.Lock()
	defer urp.mu.Unlock()
	return urp.refreshAll()
}

// refreshAll calls the urp's Renter's managedBubbleMetadata method on all the
// directories in the childDir map
func (urp *uniqueRefreshPaths) refreshAll() (err error) {
	// Create a siaPath channel with numBubbleWorkerThreads spaces
	siaPathChan := make(chan modules.SiaPath, numBubbleWorkerThreads)

	// Launch worker groups
	var wg sync.WaitGroup
	var errMU sync.Mutex
	for i := 0; i < numBubbleWorkerThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for siaPath := range siaPathChan {
				bubbleErr := urp.r.managedBubbleMetadata(siaPath)
				errMU.Lock()
				err = errors.Compose(err, bubbleErr)
				errMU.Unlock()
			}
		}()
	}

	// Add all child dir siaPaths to the siaPathChan
	for sp := range urp.childDirs {
		siaPathChan <- sp
	}

	// Close siaPathChan and wait for worker groups to complete
	close(siaPathChan)
	wg.Wait()

	return
}
