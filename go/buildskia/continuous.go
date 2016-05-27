package buildskia

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/skia-dev/glog"
	"go.skia.org/infra/go/metrics2"
	"go.skia.org/infra/go/util"
	"go.skia.org/infra/go/vcsinfo"
)

const (
	GOOD_BUILDS_FILENAME = "goodbuilds.txt"

	// PRESERVE_DURATION is used to determine if an LKGR commit should be
	// preserved.  i.e. if a the distance between two commits is greater than
	// PRESERVER_DURATION then they both should be preserved.
	PRESERVE_DURATION = 30 * 24 * time.Hour

	// DECIMATION_PERIOD is the time between decimation runs.
	DECIMATION_PERIOD = time.Hour

	// BUILD_TYPE is the type of build we use throughout.
	BUILD_TYPE = RELEASE_BUILD
)

// errors
var (
	AlreadyExistsErr = errors.New("Checkout already exists.")
)

// PerBuild is a callback function where ContinuousBuilder clients can
// perform specific builds within the newest Skia checkout.
type PerBuild func(checkout, depotTools string) error

// ContinuousBuilder is for building versions of the Skia library and then compiling and
// running command-line apps against those built versions.
//
// For each LKGR of Skia, that version of code will be checked out under
// workDir/"versions"/<git hash>.
type ContinuousBuilder struct {
	workRoot   string
	depotTools string
	repo       vcsinfo.VCS
	perBuild   PerBuild
	preserve   int

	// hashes is a cache of the hashes returned from Available.
	hashes []string

	// current is the current commit we are building at.
	current *vcsinfo.LongCommit

	// mutex protects access to hashes, current, and GOOD_BUILDS_FILENAME.
	mutex sync.Mutex
}

// New returns a new ContinuousBuilder instance.
//
//    workRoot - The root directory where work is stored.
//    depotTools - The directory where depot_tools is checked out.
//    repo - A vcs to pull hash info from.
//    perBuild - A PerBuild callback that gets called every time a new successful build of Skia is available.
func New(workRoot, depotTools string, repo vcsinfo.VCS, perBuild PerBuild, preserve int) *ContinuousBuilder {
	b := &ContinuousBuilder{
		workRoot:   workRoot,
		depotTools: depotTools,
		repo:       repo,
		perBuild:   perBuild,
		preserve:   preserve,
	}
	_, _ = b.AvailableBuilds() // Called for side-effect of loading hashes.
	go b.startDecimation()
	b.updateCurrent()

	return b
}

// prepDirectory adds the 'versions' directory to the workRoot
// and returns the full path of that directory.
func prepDirectory(workRoot string) (string, error) {
	versions := path.Join(workRoot, "versions")
	if err := os.MkdirAll(versions, 0777); err != nil {
		return "", fmt.Errorf("Failed to create WORK_ROOT/versions dir: %s", err)
	}
	return versions, nil
}

// BuildLatestSkia builds the LKGR of Skia in the given workRoot directory.
//
// The library will be checked out into workRoot + "/" + githash, where githash
// is the githash of the LKGR of Skia.
//
//    force - If true then checkout and build even if the directory already exists.
//    head - If true then build Skia at HEAD, otherwise build Skia at LKGR.
//    deps - If true then install Skia dependencies.
//
// Returns the commit info for the revision of Skia checked out.
// Returns an error if any step fails, or return AlreadyExistsErr if
// the target checkout directory already exists and force is false.
func (b *ContinuousBuilder) BuildLatestSkia(force bool, head bool, deps bool) (*vcsinfo.LongCommit, error) {
	versions, err := prepDirectory(b.workRoot)
	if err != nil {
		return nil, err
	}

	githash := ""
	if head {
		if githash, err = GetSkiaHead(nil); err != nil {
			return nil, fmt.Errorf("Failed to retrieve Skia HEAD: %s", err)
		}
	} else {
		if githash, err = GetSkiaHash(nil); err != nil {
			return nil, fmt.Errorf("Failed to retrieve Skia LKGR: %s", err)
		}
	}
	checkout := path.Join(versions, githash)

	fi, err := os.Stat(checkout)
	// If the file is present and a directory then only proceed if 'force' is true.
	if err == nil && fi.IsDir() == true && !force {
		glog.Infof("Dir already exists: %s", checkout)
		return nil, AlreadyExistsErr
	}

	ret, err := DownloadSkia("", githash, checkout, b.depotTools, false, deps)
	if err != nil {
		return nil, fmt.Errorf("Failed to fetch: %s", err)
	}

	if b.perBuild != nil {
		if err := b.perBuild(checkout, b.depotTools); err != nil {
			return nil, err
		}
	}
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.hashes = append(b.hashes, githash)
	b.updateCurrent()
	fb, err := os.OpenFile(filepath.Join(b.workRoot, GOOD_BUILDS_FILENAME), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("Failed to open %s for writing: %s", GOOD_BUILDS_FILENAME, err)
	}
	defer util.Close(fb)
	_, err = fmt.Fprintf(fb, "%s\n", githash)
	if err != nil {
		return nil, fmt.Errorf("Failed to write %s: %s", GOOD_BUILDS_FILENAME, err)
	}
	return ret, nil
}

// updateCurrent updates the value of b.current with the new gitinfo for the most recent build.
//
// Or a mildly informative stand-in if somehow the update fails.
//
// updateCurrent presumes the caller already has a lock on the mutex.
func (b *ContinuousBuilder) updateCurrent() {
	fallback := &vcsinfo.LongCommit{ShortCommit: &vcsinfo.ShortCommit{Hash: "unknown"}}
	if len(b.hashes) == 0 {
		glog.Errorf("There are no hashes.")
		if b.current == nil {
			b.current = fallback
		}
		return
	}
	details, err := b.repo.Details(b.hashes[len(b.hashes)-1], true)
	if err != nil {
		glog.Errorf("Unable to retrieve build info: %s", err)
		if b.current == nil {
			b.current = fallback
		}
		return
	}
	b.current = details
}

// AvailableBuilds returns a list of git hashes, all the versions of Skia that
// can be built against. This returns the list with the newest builds last.
// The list will always be of length > 1, otherwise and error is returned.
func (b *ContinuousBuilder) AvailableBuilds() ([]string, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	if len(b.hashes) > 0 {
		return b.hashes, nil
	}
	fi, err := os.Open(filepath.Join(b.workRoot, GOOD_BUILDS_FILENAME))
	if err != nil {
		return nil, fmt.Errorf("Failed to open %s for reading: %s", GOOD_BUILDS_FILENAME, err)
	}
	defer util.Close(fi)
	buf, err := ioutil.ReadAll(fi)
	if err != nil {
		return nil, fmt.Errorf("Failed to read: %s", err)
	}
	hashes := strings.Split(string(buf), "\n")
	realHashes := []string{}
	for _, h := range hashes {
		if h != "" {
			realHashes = append(realHashes, h)
		}
	}
	b.hashes = realHashes
	if len(b.hashes) == 0 {
		return nil, fmt.Errorf("List of available builds is empty.")
	}
	return realHashes, nil
}

func (b *ContinuousBuilder) Current() *vcsinfo.LongCommit {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.current
}

func (b *ContinuousBuilder) writeNewGoodBuilds(hashes []string) error {
	if len(hashes) < 1 {
		return fmt.Errorf("At least one good build must be kept around.")
	}
	b.mutex.Lock()
	defer b.mutex.Unlock()

	b.hashes = hashes
	fb, err := os.Create(filepath.Join(b.workRoot, GOOD_BUILDS_FILENAME))
	if err != nil {
		return fmt.Errorf("Failed to open %s for writing: %s", GOOD_BUILDS_FILENAME, err)
	}
	defer util.Close(fb)
	if _, err := fb.Write([]byte(strings.Join(hashes, "\n") + "\n")); err != nil {
		return fmt.Errorf("Failed to write %s: %s", GOOD_BUILDS_FILENAME, err)
	}
	return nil
}

func (b *ContinuousBuilder) startDecimation() {
	decimateLiveness := metrics2.NewLiveness("decimate")
	decimateFailures := metrics2.GetCounter("decimate-failed", nil)
	for _ = range time.Tick(DECIMATION_PERIOD) {
		hashes, err := b.AvailableBuilds()
		if err != nil {
			glog.Errorf("Failed to get available builds while decimating: %s", err)
			decimateFailures.Inc(1)
			continue
		}
		keep, remove, err := decimate(hashes, b.repo, b.preserve)
		if err != nil {
			glog.Errorf("Failed to calc removals while decimating: %s", err)
			decimateFailures.Inc(1)
			continue
		}
		glog.Infof("Decimate: Keep %v Remove %v", keep, remove)
		for _, hash := range remove {
			glog.Infof("Decimate: Beginning %s", hash)
			if err := os.RemoveAll(filepath.Join(b.workRoot, "versions", hash)); err != nil {
				glog.Errorf("Failed to remove directory for %s: %s", hash, err)
				continue
			}
			glog.Infof("Decimate: Finished %s", hash)
		}
		if err := b.writeNewGoodBuilds(keep); err != nil {
			continue
		}
		decimateFailures.Reset()
		decimateLiveness.Reset()
	}
}

// decimate returns a list of hashes to keep, the list to remove,
// and an error if one occurred.
//
// The algorithm is:
//   Preserve all hashes that are spaced one month apart.
//   Then if there are more than 'limit' remaining hashes
//   remove every other one to bring the count down to 'limit'/2.
//
func decimate(hashes []string, vcs vcsinfo.VCS, limit int) ([]string, []string, error) {
	keep := []string{}
	remove := []string{}

	// The hashes are stored with the oldest first, newest last.
	// So we start at the front and work forward until we start to find hashes that are less than
	// PRESERVE_DURATION apart. Once we find that spot set oldiesEnd
	// to that index.
	oldiesEnd := 0
	c, err := vcs.Details(hashes[0], true)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to get hash details: %s", err)
	}
	lastTS := time.Time{}
	for i, h := range hashes {
		c, err = vcs.Details(h, true)
		if err != nil {
			return nil, nil, fmt.Errorf("Failed to get hash details: %s", err)
		}
		fmt.Printf("%v", c.Timestamp.Sub(lastTS))
		if c.Timestamp.Sub(lastTS) < PRESERVE_DURATION {
			break
		}
		lastTS = c.Timestamp
		oldiesEnd = i
	}

	// Now that we know where the old hashes that we want to preserve are, we
	// will chop them off and ignore them for the rest of the decimation process.
	oldies := hashes[:oldiesEnd]
	hashes = hashes[oldiesEnd:]
	fmt.Println(oldies, hashes)

	// Only do decimation if we have enough fresh hashes.
	if len(hashes) < limit {
		return append(oldies, hashes...), remove, nil
	}
	last := hashes[len(hashes)-1]
	for i, h := range hashes[:len(hashes)-1] {
		if i%2 == 0 {
			keep = append(keep, h)
		} else {
			remove = append(remove, h)
		}
	}
	keep = append(keep, last)
	// Once done with decimation add the oldies back into the list of hashes to keep.
	return append(oldies, keep...), remove, nil
}