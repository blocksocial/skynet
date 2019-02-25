package renter

import (
	"sync"
	"testing"
	"time"

	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/siatest"
	"gitlab.com/NebulousLabs/fastrand"
)

// TestStresstestSiaFileSet is a vlong test that performs multiple operations
// which modify the siafileset in parallel for a period of time.
func TestStresstestSiaFileSet(t *testing.T) {
	if testing.Short() { // || !build.VLONG {
		t.SkipNow()
	}
	// Create a group for the test.
	groupParams := siatest.GroupParams{
		Hosts:   5,
		Renters: 1,
		Miners:  1,
	}
	tg, err := siatest.NewGroupFromTemplate(renterTestDir(t.Name()), groupParams)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	// Run the test for a set amount of time.
	timer := time.NewTimer(time.Minute)
	stop := make(chan struct{})
	go func() {
		<-timer.C
		close(stop)
	}()
	wg := new(sync.WaitGroup)
	r := tg.Renters()[0]
	// Upload params.
	dataPieces := uint64(1)
	parityPieces := uint64(1)
	// One thread uploads new files.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _, err := r.UploadNewFileBlocking(int(modules.SectorSize)+siatest.Fuzz(), dataPieces, parityPieces, false)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Duration(fastrand.Intn(1000))*time.Millisecond + time.Second) // between 1s and 2s
		}
	}()
	// One thread force uploads new files to an existing siapath.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Get existing files and choose one randomly.
			files, err := r.Files()
			if err != nil {
				t.Fatal(err)
			}
			// If there are no files we try again later.
			if len(files) == 0 {
				time.Sleep(time.Second)
				continue
			}
			sp := files[fastrand.Intn(len(files))].SiaPath
			lf, err := r.FilesDir().NewFile(int(modules.SectorSize) + siatest.Fuzz())
			if err != nil {
				t.Fatal(err)
			}
			err = r.RenterUploadForcePost(lf.Path(), sp, dataPieces, parityPieces, true)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Duration(fastrand.Intn(4000))*time.Millisecond + time.Second) // between 4s and 5s
		}
	}()
	// One thread renames files.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	// One thread deletes files.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	// One thread kills hosts to trigger repairs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	// Wait until threads are done.
	wg.Wait()
}
