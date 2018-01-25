package main

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"runtime"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"go.skia.org/infra/go/android_skia_checkout"
	"go.skia.org/infra/go/auth"
	"go.skia.org/infra/go/common"
	sk_exec "go.skia.org/infra/go/exec"
	"go.skia.org/infra/go/fileutil"
	"go.skia.org/infra/go/git"
	"go.skia.org/infra/go/metrics2"
	"go.skia.org/infra/go/sklog"
	"go.skia.org/infra/go/timer"
	"go.skia.org/infra/go/util"
)

const (
	CHECKOUTS_TOPLEVEL_DIR = "checkouts"
	CHECKOUT_DIR_PREFIX    = "checkout"

	ANDROID_MANIFEST_URL = "https://googleplex-android.googlesource.com/a/platform/manifest"
	ANDROID_SKIA_URL     = "https://googleplex-android.googlesource.com/a/platform/external/skia"

	TRY_BRANCH_NAME = "try_branch"

	COMPILE_TASK_LOGS_BUCKET = "android-compile-logs"
)

var (
	availableCheckoutsChan chan string

	bucketHandle *storage.BucketHandle
)

func CheckoutsInit(numCheckouts int, workdir string) error {
	// Slice that will be used to update all checkouts in parallel.
	checkoutsToUpdate := []string{}
	// Channel that will be used to determine which checkouts are available.
	availableCheckoutsChan = make(chan string, numCheckouts)
	// Populate the channel with available checkouts.
	for i := 1; i <= numCheckouts; i++ {
		checkoutPath := filepath.Join(workdir, CHECKOUTS_TOPLEVEL_DIR, fmt.Sprintf("%s_%d", CHECKOUT_DIR_PREFIX, i))
		if _, err := fileutil.EnsureDirExists(checkoutPath); err != nil {
			return fmt.Errorf("Error creating %s: %s", checkoutPath, err)
		}
		checkoutsToUpdate = append(checkoutsToUpdate, checkoutPath)
		addToCheckoutsChannel(checkoutPath)
	}

	// Update all checkouts simultaneously.
	if err := updateCheckoutsInParallel(checkoutsToUpdate); err != nil {
		return fmt.Errorf("Error when updating checkouts in parallel: %s", err)
	}

	// Get a handle to the Google Storage bucket that logs will be stored in.
	client, err := auth.NewDefaultJWTServiceAccountClient(auth.SCOPE_READ_WRITE)
	if err != nil {
		return fmt.Errorf("Problem setting up client OAuth: %s", err)
	}
	storageClient, err := storage.NewClient(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		return fmt.Errorf("Failed to create a Google Storage API client: %s", err)
	}
	bucketHandle = storageClient.Bucket(COMPILE_TASK_LOGS_BUCKET)
	return nil
}

// UpdateCheckoutsInParallel updates all specified checkouts in parallel.
func updateCheckoutsInParallel(checkouts []string) error {
	sklog.Infof("About to update %d checkouts in parallel.", len(checkouts))
	sklog.Info("If the server has no existing checkouts then this step could take a while...")
	ctx := context.Background()
	group := util.NewNamedErrGroup()
	for _, checkout := range checkouts {
		c := checkout // https://golang.org/doc/faq#closures_and_goroutines
		// Create and run a goroutine closure that updates the checkout.
		group.Go(c, func() error {
			// Make sure the Skia checkout (if it exists) is without any modifications
			// from a previous interrupted run.
			skiaPath := filepath.Join(c, "external", "skia")
			if stat, err := os.Stat(skiaPath); err == nil && stat.IsDir() {
				skiaCheckout, err := git.NewCheckout(ctx, ANDROID_SKIA_URL, path.Dir(skiaPath))
				if err != nil {
					return fmt.Errorf("Failed to create GitDir from %s: %s", skiaPath, err)
				}
				if err := cleanSkiaCheckout(ctx, skiaCheckout, c); err != nil {
					return fmt.Errorf("Error when cleaning Skia checkout at %s: %s", skiaCheckout.Dir(), err)
				}
			}
			// Now update the Android checkout.
			if err := updateCheckout(ctx, c); err != nil {
				return fmt.Errorf("Error when updating checkout in %s: %s", c, err)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	sklog.Infof("Done updating %d checkouts in parallel.", len(checkouts))
	return nil
}

// updateCheckout updates the Android checkout using the repo tool in the
// specified checkout.
func updateCheckout(ctx context.Context, checkoutPath string) error {
	checkoutBase := path.Base(checkoutPath)
	sklog.Infof("Started updating %s", checkoutBase)
	// Create metric and send it to a timer.
	syncTimesMetric := metrics2.GetFloat64Metric(fmt.Sprintf("android_sync_time_%s", checkoutBase))
	defer timer.NewWithMetric(fmt.Sprintf("Time taken to update %s:", checkoutBase), syncTimesMetric).Stop()

	user, err := user.Current()
	if err != nil {
		return err
	}
	repoToolPath := path.Join(user.HomeDir, "bin", "repo")
	// Run repo init and sync commands.
	if _, err := sk_exec.RunCwd(ctx, checkoutPath, repoToolPath, "init", "-u", ANDROID_MANIFEST_URL, "-g", "all,-notdefault,-darwin", "-b", "master"); err != nil {
		return fmt.Errorf("Failed to init the repo at %s: %s", checkoutBase, err)
	}
	// Sync the current branch.
	if _, err := sk_exec.RunCwd(ctx, checkoutPath, repoToolPath, "sync", "-c", "-j32"); err != nil {
		return fmt.Errorf("Failed to sync the repo at %s: %s", checkoutBase, err)
	}

	return nil
}

// applyPatch applies a patch from the specified issue and patchset to the
// specified Skia checkout.
func applyPatch(ctx context.Context, skiaCheckout *git.Checkout, issue, patchset int) error {
	// Run a git fetch for the branch where gerrit stores patches.
	//
	//  refs/changes/46/4546/1
	//                |  |   |
	//                |  |   +-> Patch set.
	//                |  |
	//                |  +-> Issue ID.
	//                |
	//                +-> Last two digits of Issue ID.
	issuePostfix := issue % 100
	if err := skiaCheckout.FetchRefFromRepo(ctx, common.REPO_SKIA, fmt.Sprintf("refs/changes/%02d/%d/%d", issuePostfix, issue, patchset)); err != nil {
		return fmt.Errorf("Failed to fetch ref in %s: %s", skiaCheckout.Dir(), err)
	}
	if _, err := skiaCheckout.Git(ctx, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("Failed to checkout FETCH_HEAD in %s: %s", skiaCheckout.Dir(), err)
	}
	if _, err := skiaCheckout.Git(ctx, "rebase"); err != nil {
		return fmt.Errorf("Failed to rebase in %s: %s", skiaCheckout.Dir(), err)
	}
	return nil
}

func deleteTryBranch(ctx context.Context, skiaCheckout *git.Checkout) error {
	if _, err := skiaCheckout.Git(ctx, "branch", "-D", TRY_BRANCH_NAME); err != nil {
		return fmt.Errorf("Failed to delete branch %s: %s", TRY_BRANCH_NAME, err)
	}
	return nil
}

func resetSkiaCheckout(ctx context.Context, skiaCheckout *git.Checkout, resetCommit string) error {
	if _, err := skiaCheckout.Git(ctx, "clean", "-d", "-f"); err != nil {
		return err
	}
	if _, err := skiaCheckout.Git(ctx, "reset", "--hard", resetCommit); err != nil {
		return err
	}
	return nil
}

func cleanSkiaCheckout(ctx context.Context, skiaCheckout *git.Checkout, checkoutPath string) error {
	if err := resetSkiaCheckout(ctx, skiaCheckout, "HEAD"); err != nil {
		return fmt.Errorf("Error when resetting Skia checkout: %s", err)
	}
	// Checkout Android's built in remote "goog/master" incase the checkout is
	// tracking something else.
	if _, err := skiaCheckout.Git(ctx, "checkout", fmt.Sprintf("%s/master", android_skia_checkout.BUILT_IN_REMOTE)); err != nil {
		return fmt.Errorf("Failed to checkout goog/master in %s: %s", checkoutPath, err)
	}
	if err := deleteTryBranch(ctx, skiaCheckout); err != nil {
		// Do nothing. The try branch does not exist yet which is ok.
	}
	return nil
}

func prepareSkiaCheckoutForCompile(ctx context.Context, userConfigContent []byte, skiaCheckout *git.Checkout) error {
	skiaPath := skiaCheckout.Dir()
	if err := android_skia_checkout.RunGnToBp(ctx, skiaPath); err != nil {
		return fmt.Errorf("Error when running gn_to_bp: %s", err)
	}
	// TODO(rmistry): Cannot compile Android with third_party/externals. Find
	// a relatively less hackier way around this.
	if err := os.RemoveAll(filepath.Join(skiaPath, "third_party", "externals")); err != nil {
		// go back to previous state.
		return fmt.Errorf("Error when deleting third_party/externals: %s", err)
	}
	// Create SkUserConfigManual.h file since it is required for compilation.
	f, err := os.Create(filepath.Join(skiaPath, android_skia_checkout.SkUserConfigManualRelPath))
	if err != nil {
		return fmt.Errorf("Could not create %s: %s", android_skia_checkout.SkUserConfigManualRelPath, err)
	}
	defer util.Close(f)
	if _, err := f.Write(userConfigContent); err != nil {
		return fmt.Errorf("Could not write to %s: %s", android_skia_checkout.SkUserConfigManualRelPath, err)
	}

	return nil
}

func addToCheckoutsChannel(checkout string) {
	availableCheckoutsChan <- checkout
}

// RunCompileTask runs the specified CompileTask using the following algorithm-
//
// Step 1: Find an available Android checkout and update the CompileTask with
// the checkout. This is done for the UI and for easier debugging.
//
// Step 2: Make sure the Skia checkout within Android is clean. We do this
// before updating to be extra careful and make the server more robust.
//
// Step 3: Update the Android checkout.
//
// Step 4: Get the Android hash currently in Skia's checkout. We will
// go back to this hash after we are done updating to Skia master/origin
// and compiling.
//
// Step 5: Create a branch and have it track origin/master.
//
// Step 6: If it is a trybot run then apply the patch else apply the hash.
//
// Step 7: Prepare the Skia checkout for compilation: Run gn_to_bp.py and
// create SkUserConfigManual.h from Step 5.
//
// Step 8: Do the with patch or with hash compilation and update CompileTask
// with link to logs and whether it was successful.
//
// Step 9: If the compilation failed and if it is a trybot run then verify
// that the tree is not broken by building at Skia HEAD. Update CompileTask
// with link to logs and whether the no patch run was successful.
//
func RunCompileTask(ctx context.Context, task *CompileTask, datastoreKey *datastore.Key) error {
	// Blocking call to wait for an available checkout.
	checkoutPath := <-availableCheckoutsChan
	defer addToCheckoutsChannel(checkoutPath)

	// Step 1: Find an available Android checkout and update the CompileTask
	// with the checkout. This is done for the UI and for easier debugging.
	task.Checkout = path.Base(checkoutPath)
	if _, err := UpdateDSTask(ctx, datastoreKey, task); err != nil {
		return fmt.Errorf("Could not update compile task with ID %d: %s", datastoreKey.ID, err)
	}

	skiaPath := filepath.Join(checkoutPath, "external", "skia")
	skiaCheckout, err := git.NewCheckout(ctx, ANDROID_SKIA_URL, path.Dir(skiaPath))
	if err != nil {
		return fmt.Errorf("Failed to create GitDir from %s: %s", skiaPath, err)
	}

	// Step 2: Make sure the Skia checkout within Android is clean. We do this
	// before updating to be extra careful and make the server more robust.
	if err := cleanSkiaCheckout(ctx, skiaCheckout, checkoutPath); err != nil {
		return fmt.Errorf("Error when cleaning Skia checkout: %s", err)
	}

	// Step 3: Update the Android checkout.
	if err := updateCheckout(ctx, checkoutPath); err != nil {
		return fmt.Errorf("Error when updating checkout in %s: %s", checkoutPath, err)
	}

	// Step 4: Get contents of SkUserConfigManual.h. We will use this after
	// updating Skia to master/origin and before compiling.
	userConfigContent, err := ioutil.ReadFile(filepath.Join(skiaPath, android_skia_checkout.SkUserConfigManualRelPath))
	if err != nil {
		return fmt.Errorf("Could not read from %s: %s", android_skia_checkout.SkUserConfigManualRelPath, err)
	}

	// Add origin remote that points to the Skia repo.
	if err := skiaCheckout.AddRemote(ctx, "origin", common.REPO_SKIA); err != nil {
		return fmt.Errorf("Error when adding origin remote: %s", err)
	}
	// Fetch origin without updating checkout
	if err := skiaCheckout.Fetch(ctx); err != nil {
		return fmt.Errorf("Error when fetching origin: %s", err)
	}

	// Step 5: Create a branch and have it track origin/master.
	if _, err := skiaCheckout.Git(ctx, "checkout", "-b", TRY_BRANCH_NAME, "-t", "origin/master"); err != nil {
		return fmt.Errorf("Error when creating %s in %s: %s", TRY_BRANCH_NAME, skiaPath, err)
	}

	// Step 6:  If it is a trybot run then apply the patch else apply the hash.
	trybotRun := (task.Hash == "")
	if trybotRun {
		// Apply Patch.
		if err := applyPatch(ctx, skiaCheckout, task.Issue, task.PatchSet); err != nil {
			return fmt.Errorf("Could not apply the patch with issue %d and patchset %d: %s", task.Issue, task.PatchSet, err)
		}
	} else {
		// Checkout the specified Skia hash.
		// TODO(rmistry): This has lots of problems, the non-trybot bot could fail if
		// Android tree is red. Maybe non-trybot path should not be supported?
		if _, err := skiaCheckout.Git(ctx, "checkout", task.Hash); err != nil {
			return fmt.Errorf("Failed to checkout Skia hash %s: %s", task.Hash, err)
		}
	}

	// Step 7: Prepare the Skia checkout for compilation: Run gn_to_bp.py and
	// create SkUserConfigManual.h from Step 5.
	if err := prepareSkiaCheckoutForCompile(ctx, userConfigContent, skiaCheckout); err != nil {
		return fmt.Errorf("Could not prepare Skia checkout for compile: %s", err)
	}

	// Step 8: Do the with patch or with hash compilation and update CompileTask
	// with link to logs and whether it was successful.
	withPatchSuccess, gsWithPatchLink, err := compileCheckout(ctx, checkoutPath, fmt.Sprintf("%d_withpatch_", datastoreKey.ID))
	if err != nil {
		return fmt.Errorf("Error when compiling checkout withpatch at %s: %s", checkoutPath, err)
	}
	task.WithPatchSucceeded = withPatchSuccess
	task.WithPatchLog = gsWithPatchLink
	if _, err := UpdateDSTask(ctx, datastoreKey, task); err != nil {
		return fmt.Errorf("Could not update compile task with ID %d: %s", datastoreKey.ID, err)
	}

	// Step 9: If the compilation failed and if it is a trybot run then verify
	// that the tree is not broken by building at Skia HEAD. Update CompileTask
	// with link to logs and whether the no patch run was successful.
	if !withPatchSuccess && trybotRun {
		// If this failed then check to see if a build without the patch will succeed.
		if err := resetSkiaCheckout(ctx, skiaCheckout, "origin/master"); err != nil {
			return fmt.Errorf("Error when resetting Skia checkout: %s", err)
		}
		// Checkout origin/master.
		if _, err := skiaCheckout.Git(ctx, "checkout", "origin/master"); err != nil {
			return fmt.Errorf("Failed to checkout origin/master: %s", err)
		}
		if err := prepareSkiaCheckoutForCompile(ctx, userConfigContent, skiaCheckout); err != nil {
			return fmt.Errorf("Could not prepare Skia checkout for compile: %s", err)
		}
		// Do the no patch compilation.
		noPatchSuccess, gsNoPatchLink, err := compileCheckout(ctx, checkoutPath, fmt.Sprintf("%d_nopatch_", datastoreKey.ID))
		if err != nil {
			return fmt.Errorf("Error when compiling checkout nopatch at %s: %s", checkoutPath, err)
		}
		task.NoPatchSucceeded = noPatchSuccess
		task.NoPatchLog = gsNoPatchLink
		if _, err := UpdateDSTask(ctx, datastoreKey, task); err != nil {
			return fmt.Errorf("Could not update compile task with ID %d: %s", datastoreKey.ID, err)
		}
	}

	return nil
}

// compileCheckout runs compile.sh which compiles the provided Android checkout.
// Compile logs are then stored in the provided Google storage location with the
// specified prefix. Returns whether the compilation was successful and a link
// to the compilation logs in Google Storage.
// We do the compilation via compile.sh and not via exec because
// ./build/envsetup.sh needs to be sournced before running lunch and mma
// commands and this was much simpler to do via a bash script.
func compileCheckout(ctx context.Context, checkoutPath, logFilePrefix string) (bool, string, error) {
	checkoutBase := path.Base(checkoutPath)
	sklog.Infof("Started compiling %s", checkoutBase)
	// Create metric and send it to a timer.
	compileTimesMetric := metrics2.GetFloat64Metric(fmt.Sprintf("android_compile_time_%s", checkoutBase))
	defer timer.NewWithMetric(fmt.Sprintf("Time taken to compile %s:", checkoutBase), compileTimesMetric).Stop()

	// Find the compile script.
	_, currentFile, _, _ := runtime.Caller(0)
	compileScript := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(currentFile))), "compile.sh")
	// Execute the compile script pointing it to the checkout.
	command := exec.Command(compileScript, checkoutPath)
	logFile, err := ioutil.TempFile(*workdir, logFilePrefix)
	defer util.Remove(logFile.Name())
	if err != nil {
		return false, "", fmt.Errorf("Could not create log file")
	}
	command.Stdout = io.MultiWriter(logFile, os.Stdout)
	command.Stderr = command.Stdout

	// Execute the command and determine if it was successful
	compileSuccess := (command.Run() == nil)

	// Put the log file in Google Storage.
	target := bucketHandle.Object(filepath.Base(logFile.Name()))
	writer := target.NewWriter(ctx)
	writer.ObjectAttrs.ContentType = "text/plain"
	// Make uploaded logs readable by google.com domain.
	writer.ObjectAttrs.ACL = []storage.ACLRule{{Entity: "domain-google.com", Role: storage.RoleReader}}
	defer util.Close(writer)

	f, err := os.Open(logFile.Name())
	defer f.Close()
	// Write the actual data.
	if _, err := io.Copy(writer, f); err != nil {
		return compileSuccess, "", fmt.Errorf("Could not write %s to google storage: %s", logFile.Name(), err)
	}
	return compileSuccess, fmt.Sprintf("https://storage.cloud.google.com/%s/%s", COMPILE_TASK_LOGS_BUCKET, filepath.Base(logFile.Name())), nil
}