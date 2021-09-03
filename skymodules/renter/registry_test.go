package renter

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"strings"
	"testing"
	"time"

	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/SkynetLabs/skyd/build"
	"gitlab.com/SkynetLabs/skyd/skymodules"
	"go.sia.tech/siad/crypto"
	"go.sia.tech/siad/modules"
	"go.sia.tech/siad/persist"
	"go.sia.tech/siad/types"
)

// TestReadResponseSet is a unit test for the readResponseSet.
func TestReadResponseSet(t *testing.T) {
	t.Parallel()

	// Get a set and fill it up completely.
	n := 10
	c := make(chan *jobReadRegistryResponse)
	set := newReadResponseSet(c, n)
	go func() {
		for i := 0; i < n; i++ {
			c <- &jobReadRegistryResponse{staticErr: fmt.Errorf("%v", i)}
		}
	}()
	if set.responsesLeft() != n {
		t.Fatal("wrong number of responses left", set.responsesLeft(), n)
	}

	// Calling Next should work until it's empty.
	i := 0
	for set.responsesLeft() > 0 {
		resp := set.next(context.Background())
		if resp == nil {
			t.Fatal("resp shouldn't be nil")
		}
		if resp.staticErr.Error() != fmt.Sprint(i) {
			t.Fatal("wrong error", resp.staticErr, fmt.Sprint(i))
		}
		i++
	}

	// Call Next one more time and close the context while doing so.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp := set.next(ctx)
	if resp != nil {
		t.Fatal("resp should be nil")
	}

	// Collect all values.
	resps := set.collect(context.Background())
	for i, resp := range resps {
		if resp.staticErr.Error() != fmt.Sprint(i) {
			t.Fatal("wrong error", resp.staticErr, fmt.Sprint(i))
		}
	}

	// Create another set that is collected right away.
	c = make(chan *jobReadRegistryResponse)
	set = newReadResponseSet(c, n)
	go func() {
		for i := 0; i < n; i++ {
			c <- &jobReadRegistryResponse{staticErr: fmt.Errorf("%v", i)}
		}
	}()
	resps = set.collect(context.Background())
	for i, resp := range resps {
		if resp.staticErr.Error() != fmt.Sprint(i) {
			t.Fatal("wrong error", resp.staticErr, fmt.Sprint(i))
		}
	}

	// Create another set that is collected halfway and then cancelled.
	c = make(chan *jobReadRegistryResponse)
	set = newReadResponseSet(c, n/2)
	ctx, cancel = context.WithCancel(context.Background())
	go func(cancel context.CancelFunc) {
		for i := 0; i < n/2; i++ {
			c <- &jobReadRegistryResponse{staticErr: fmt.Errorf("%v", i)}
		}
		cancel()
	}(cancel)
	resps = set.collect(ctx)
	if len(resps) != n/2 {
		t.Fatal("wrong number of resps", len(resps), n/2)
	}
	for i, resp := range resps {
		if resp.staticErr.Error() != fmt.Sprint(i) {
			t.Fatal("wrong error", resp.staticErr, fmt.Sprint(i))
		}
	}

	// Collect a set without responses with a closed context.
	set = newReadResponseSet(c, n)
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	resps = set.collect(ctx)
	if len(resps) != 0 {
		t.Fatal("resps should be empty", resps)
	}
}

// TestThreadedAddResponseSetRetry tests that threadedAddResponseSet will try to
// fetch the retrieved revision from other workers to prevent slow hosts that
// are updated from skewing the stats.
func TestThreadedAddResponseSetRetry(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create a renter.
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rt.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Add 4 hosts.
	var hosts []modules.Host
	for i := 0; i < 4; i++ {
		h, err := rt.addHost(fmt.Sprintf("host%v", i))
		if err != nil {
			t.Fatal(err)
		}
		hosts = append(hosts, h)
	}
	// Close 3 of them at the end of the test.
	for i := 0; i < len(hosts)-1; i++ {
		defer func(i int) {
			if err := hosts[i].Close(); err != nil {
				t.Fatal(err)
			}
		}(i)
	}

	// Set an allowance.
	err = rt.renter.staticHostContractor.SetAllowance(skymodules.DefaultAllowance)
	if err != nil {
		t.Fatal(err)
	}

	// Wait until we got 4 workers in the pool.
	numRetries := 0
	err = build.Retry(100, 100*time.Millisecond, func() error {
		if numRetries%10 == 0 {
			_, err = rt.miner.AddBlock()
			if err != nil {
				t.Fatal(err)
			}
		}
		numRetries++
		workers := rt.renter.staticWorkerPool.callWorkers()
		if len(workers) != len(hosts) {
			return fmt.Errorf("%v != %v", len(workers), len(hosts))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create a random registry entry and a higher revision.
	srvLower, spk, sk := randomRegistryValue()
	srvHigher := srvLower
	srvHigher.Revision++
	srvHigher = srvHigher.Sign(sk)
	entryLower := skymodules.NewRegistryEntry(spk, srvLower)
	entryHigher := skymodules.NewRegistryEntry(spk, srvHigher)

	// Get workers for the corresponding hosts.
	w1, err1 := rt.renter.staticWorkerPool.callWorker(hosts[0].PublicKey())
	w2, err2 := rt.renter.staticWorkerPool.callWorker(hosts[1].PublicKey())
	w3, err3 := rt.renter.staticWorkerPool.callWorker(hosts[2].PublicKey())
	w4, err4 := rt.renter.staticWorkerPool.callWorker(hosts[3].PublicKey())
	err = errors.Compose(err1, err2, err3, err4)
	if err != nil {
		t.Fatal(err)
	}

	// Update first two hosts with the higher revision. The rest doesn't know.
	workers := []*worker{w1, w2, w3, w4}
	for i := 0; i < 2; i++ {
		err = workers[i].UpdateRegistry(context.Background(), spk, srvHigher)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Take host 4 offline.
	if err := hosts[3].Close(); err != nil {
		t.Fatal(err)
	}

	// Create a fake response set where w1 returns the lower entry and w2, w3
	// and w4 the higher one.
	startTime := time.Now()
	c := make(chan *jobReadRegistryResponse)
	close(c)
	rrs := &readResponseSet{
		c:    c,
		left: 0,
		readResps: []*jobReadRegistryResponse{
			// Super fast response but no response value.
			{
				staticCompleteTime:        startTime.Add(time.Millisecond),
				staticSignedRegistryValue: nil, // no response
				staticWorker:              nil, // will be ignored
			},
			// Super fast response but error.
			{
				staticCompleteTime: startTime.Add(time.Millisecond),
				staticErr:          errors.New("failed"),
				staticWorker:       nil, // will be ignored
			},
			// Slow response with higher rev that will be the "best".
			{
				staticCompleteTime:        startTime.Add(2 * time.Second),
				staticSignedRegistryValue: &entryHigher,
				staticWorker:              w1,
			},
			// Faster response.
			{
				staticCompleteTime:        startTime.Add(time.Second),
				staticSignedRegistryValue: &entryLower,
				staticWorker:              w2,
			},
			// Super fast response but won't know the entry later.
			{
				staticCompleteTime:        startTime.Add(time.Millisecond),
				staticSignedRegistryValue: &entryLower,
				staticWorker:              w3,
			},
			// Super fast response but will be offline later.
			{
				staticCompleteTime:        startTime.Add(time.Millisecond),
				staticSignedRegistryValue: &entryLower,
				staticWorker:              w4,
			},
		},
	}

	// Create a logger.
	buf := bytes.NewBuffer(nil)
	log, err := persist.NewLogger(buf)
	if err != nil {
		t.Fatal(err)
	}

	// Reset the stats collector.
	dt := skymodules.NewDistributionTrackerStandard()
	rt.renter.staticRegReadStats = dt

	// Run the method.
	rt.renter.threadedAddResponseSet(context.Background(), testSpan(), startTime, rrs, log)

	// Check p99. The winning timing should be 1s which results in an estimate
	// of 1.02s.
	allNines := rt.renter.staticRegReadStats.Percentiles()
	p99 := allNines[0][2]
	if p99 != 1008*time.Millisecond {
		t.Fatal("wrong p99", p99)
	}

	// The buffer should contain the two messages printed when a worker either
	// failed to respond or retrieved a nil value.
	logs := buf.String()
	if strings.Count(logs, "threadedAddResponseSet: worker that successfully retrieved a registry value failed to retrieve it again") != 1 {
		t.Log("logs", logs)
		t.Fatal("didn't log first line")
	}
	if strings.Count(logs, "threadedAddResponseSet: worker that successfully retrieved a non-nil registry value returned nil") != 1 {
		t.Log("logs", logs)
		t.Fatal("didn't log second line")
	}
}

// TestIsBetterReadRegistryResponse is a unit test for isBetterReadRegistryResponse.
func TestIsBetterReadRegistryResponse(t *testing.T) {
	t.Parallel()

	registryEntry := func(revision uint64, tweak crypto.Hash) *skymodules.RegistryEntry {
		v := modules.SignedRegistryValue{
			RegistryValue: modules.NewRegistryValue(tweak, nil, revision, modules.RegistryTypeWithoutPubkey),
		}
		srv := skymodules.NewRegistryEntry(types.SiaPublicKey{}, v)
		return &srv
	}

	tests := []struct {
		existing *jobReadRegistryResponse
		new      *jobReadRegistryResponse
		result   bool
		equal    bool
	}{
		{
			existing: nil,
			new:      &jobReadRegistryResponse{},
			result:   true,
			equal:    false,
		},
		{
			existing: &jobReadRegistryResponse{},
			new:      nil,
			result:   false,
			equal:    false,
		},
		{
			existing: nil,
			new:      nil,
			result:   false,
			equal:    true,
		},
		{
			existing: &jobReadRegistryResponse{
				staticSignedRegistryValue: nil,
			},
			new: &jobReadRegistryResponse{
				staticSignedRegistryValue: &skymodules.RegistryEntry{},
			},
			result: true,
			equal:  false,
		},
		{
			existing: &jobReadRegistryResponse{
				staticSignedRegistryValue: &skymodules.RegistryEntry{},
			},
			new: &jobReadRegistryResponse{
				staticSignedRegistryValue: nil,
			},
			result: false,
			equal:  false,
		},
		{
			existing: &jobReadRegistryResponse{
				staticSignedRegistryValue: nil,
			},
			new: &jobReadRegistryResponse{
				staticSignedRegistryValue: nil,
			},
			result: false,
			equal:  true,
		},
		{
			existing: &jobReadRegistryResponse{
				staticSignedRegistryValue: registryEntry(0, crypto.Hash{}),
			},
			new: &jobReadRegistryResponse{
				staticSignedRegistryValue: registryEntry(1, crypto.Hash{}),
			},
			result: true,
			equal:  false,
		},
		{
			existing: &jobReadRegistryResponse{
				staticSignedRegistryValue: registryEntry(1, crypto.Hash{}),
			},
			new: &jobReadRegistryResponse{
				staticSignedRegistryValue: registryEntry(0, crypto.Hash{}),
			},
			result: false,
			equal:  false,
		},
		{
			existing: &jobReadRegistryResponse{
				staticSignedRegistryValue: registryEntry(0, crypto.Hash{1, 2, 3}),
			},
			new: &jobReadRegistryResponse{
				staticSignedRegistryValue: registryEntry(0, crypto.Hash{3, 2, 1}),
			},
			result: true,
			equal:  false,
		},
		{
			existing: &jobReadRegistryResponse{
				staticSignedRegistryValue: registryEntry(0, crypto.Hash{3, 2, 1}),
			},
			new: &jobReadRegistryResponse{
				staticSignedRegistryValue: registryEntry(0, crypto.Hash{1, 2, 3}),
			},
			result: false,
			equal:  false,
		},
		{
			existing: &jobReadRegistryResponse{
				staticSignedRegistryValue: registryEntry(1, crypto.Hash{}),
			},
			new: &jobReadRegistryResponse{
				staticSignedRegistryValue: registryEntry(1, crypto.Hash{}),
			},
			result: false,
			equal:  true,
		},
	}

	for i, test := range tests {
		if test.new != nil {
			test.new.staticWorker = &worker{}
		}
		result, equal := isBetterReadRegistryResponse(test.existing, test.new)
		if result != test.result {
			t.Errorf("%v: wrong result expected %v but was %v", i, test.result, result)
		}
		if equal != test.equal {
			t.Errorf("%v: wrong result expected %v but was %v", i, test.result, result)
		}
	}
}

// TestThreadedAddResponseSetBestHostIndex makes sure that we always use the 5th
// best worker response for our metrics in threadedAddResponseSet.
func TestThreadedAddResponseSetBestHostIndex(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create a renter.
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rt.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Add some hosts.
	nHosts := bestWorkerIndexForResponseSet + 2
	var hosts []modules.Host
	for i := 0; i < nHosts; i++ {
		h, err := rt.addHost(fmt.Sprintf("host%v", i))
		if err != nil {
			t.Fatal(err)
		}
		hosts = append(hosts, h)
	}

	// Set an allowance.
	a := skymodules.DefaultAllowance
	a.Hosts = uint64(nHosts)
	err = rt.renter.staticHostContractor.SetAllowance(a)
	if err != nil {
		t.Fatal(err)
	}

	// Wait until we got workers in the pool.
	numRetries := 0
	var workers []*worker
	err = build.Retry(100, 100*time.Millisecond, func() error {
		if numRetries%10 == 0 {
			_, err = rt.miner.AddBlock()
			if err != nil {
				t.Fatal(err)
			}
		}
		numRetries++
		workers = rt.renter.staticWorkerPool.callWorkers()
		if len(workers) != nHosts {
			return fmt.Errorf("%v != %v", len(workers), nHosts)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create a random entry.
	srv, spk, _ := randomRegistryValue()
	entry := skymodules.NewRegistryEntry(spk, srv)

	// Create a fake response set with 1 response.
	startTime := time.Now()
	c := make(chan *jobReadRegistryResponse)
	close(c)
	rrs := &readResponseSet{
		c:    c,
		left: 0,
		readResps: []*jobReadRegistryResponse{
			// 2s response.
			{
				staticSPK:                 &spk,
				staticTweak:               &srv.Tweak,
				staticCompleteTime:        startTime.Add(2 * time.Second),
				staticSignedRegistryValue: &entry,
				staticWorker:              workers[0],
			},
		},
	}

	// Prepare a helper to create a faster (1s) response.
	faster := func() *jobReadRegistryResponse {
		return &jobReadRegistryResponse{
			staticSPK:                 &spk,
			staticTweak:               &srv.Tweak,
			staticCompleteTime:        startTime.Add(time.Second),
			staticSignedRegistryValue: &entry,
			staticWorker:              workers[len(rrs.readResps)],
		}
	}

	// Create a logger.
	log, err := persist.NewLogger(ioutil.Discard)
	if err != nil {
		t.Fatal(err)
	}

	// Call threadedAddResponseSet in a loop. At the end of the loop we
	// append the faster response to the set.
	for i := 0; i < bestWorkerIndexForResponseSet+1; i++ {
		// Reset the stats collector.
		dt := skymodules.NewDistributionTrackerStandard()
		rt.renter.staticRegReadStats = dt

		// Add the response set.
		rt.renter.threadedAddResponseSet(context.Background(), testSpan(), startTime, rrs, log)

		// Check p99. The winning timing should be 2s which results in an estimate
		// of 2048ms.
		allNines := rt.renter.staticRegReadStats.Percentiles()
		p99 := allNines[0][2]
		if p99 != 2048*time.Millisecond {
			t.Fatal("wrong p99", p99)
		}

		// Append another response.
		rrs.readResps = append(rrs.readResps, faster())
	}

	// Run one more test. Now that we have pushed BestWorkerIndexForResponseSet+1
	// elements to rrs, the slowest timing should have been pushed out of the top 5
	// best responses. So the estimate should be faster now.
	// Reset the stats collector.
	dt := skymodules.NewDistributionTrackerStandard()
	rt.renter.staticRegReadStats = dt

	// Add the response set.
	rt.renter.threadedAddResponseSet(context.Background(), testSpan(), startTime, rrs, log)

	// Check p99. The winning timing should be 1s now since the bad timing
	// was pushed out of the bests set.
	allNines := rt.renter.staticRegReadStats.Percentiles()
	p99 := allNines[0][2]
	if p99 != 1008*time.Millisecond {
		t.Fatal("wrong p99", p99)
	}

	// Set the entry on all hosts. That way we test the secondBests code as
	// well.
	for _, worker := range workers {
		err = worker.UpdateRegistry(context.Background(), spk, srv)
		if err != nil {
			t.Fatal(err)
		}
	}

	// We shrink the response set again to make sure the slow response will
	// end up in the 5th place again. We also set the entry after the
	// slowest one to be slightly faster than it but slower than the 3 other
	// ones.
	rrs.readResps = rrs.readResps[:bestWorkerIndexForResponseSet+1]
	rrs.readResps[1].staticCompleteTime = rrs.readResps[1].staticCompleteTime.Add(500 * time.Millisecond)

	// Reset and run.
	dt = skymodules.NewDistributionTrackerStandard()
	rt.renter.staticRegReadStats = dt
	rt.renter.threadedAddResponseSet(context.Background(), testSpan(), startTime, rrs, log)

	// Check p99. This time we don't add the slowest response. That's
	// because after finding the best response, we ask all the faster
	// responses' hosts again until we have 5 secondBests. The 5th best of
	// the secondBests will be the slightly slower worker that took 1.5s.
	allNines = rt.renter.staticRegReadStats.Percentiles()
	p99 = allNines[0][2]
	if p99 != 1536*time.Millisecond {
		t.Fatal("wrong p99", p99)
	}
}
