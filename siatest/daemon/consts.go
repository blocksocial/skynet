package daemon

import (
	"os"

	"gitlab.com/NebulousLabs/Sia/persist"
	"gitlab.com/NebulousLabs/Sia/siatest"
)

// daemonTestDir creates a temporary testing directory for daemon tests. This
// should only every be called once per test. Otherwise it will delete the
// directory again.
func daemonTestDir(testName string) string {
	path := siatest.TestDir("daemon", testName)
	if err := os.MkdirAll(path, persist.DefaultDiskPermissions); err != nil {
		panic(err)
	}
	return path
}
