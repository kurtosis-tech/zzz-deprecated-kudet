package release

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/Masterminds/semver/v3"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/kurtosis-tech/stacktrace"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	gitDirname       = ".git"
	originRemoteName = "origin"
	mainBranchName   = "main"

	preReleaseScriptsFilename = ".pre-release-scripts.txt"

	tagsPrefix = "refs/tags/"
	headRef    = "refs/heads/"

	// The name of the file inside the Git directory which will store when we last fetched (in Unix seconds)
	lastFetchedFilename               = "last-fetch.txt"
	lastFetchedTimestampUintParseBase = 10
	lastFetchedTimestampUintParseBits = 64
	// How long we'll allow the user to go between fetches to ensure the repo is updated when they're releasing
	fetchGracePeriod                            = 1 * time.Minute
	extraNanosecondsToAddToLastFetchedTimestamp = 0
	lastFetchedFileMode                         = 0644

	relChangelogFilepath = "docs/changelog.md"

	// this is relative to the root of the target repo
	gitIgnoreRelFilepath      = ".gitignore"
	gitIgnoreCommentCharacter = "#"

	expectedNumTBDHeaderLines         = 1
	versionToBeReleasedPlaceholderStr = "TBD"
	sectionHeaderPrefix               = "#"
	noPreviousVersion                 = "0.0.0"
	semverRegexStr                    = "^[0-9]+.[0-9]+.[0-9]+$"

	releaseCmdStr           = "release"
	bumpMajorFlagDefaultVal = false
	bumpMajorFlagShortStr   = ""
)

var (
	versionToBeReleasedPlaceholderHeaderStr      = fmt.Sprintf("%s %s", sectionHeaderPrefix, versionToBeReleasedPlaceholderStr)
	versionToBeReleasedPlaceholderHeaderRegexStr = fmt.Sprintf("^%s\\s*%s\\s*$", sectionHeaderPrefix, versionToBeReleasedPlaceholderStr)
	versionHeaderRegexStr                        = fmt.Sprintf("^%s\\s*[0-9]+.[0-9]+.[0-9]+\\s*$", sectionHeaderPrefix)
	breakingChangesSubheaderRegexStr             = fmt.Sprintf("^%s%s%s*\\s*[Bb]reak.*$", sectionHeaderPrefix, sectionHeaderPrefix, sectionHeaderPrefix)
	semverRegex                                  = regexp.MustCompile(semverRegexStr)
	versionToBeReleasedPlaceholderHeaderRegex    = regexp.MustCompile(versionToBeReleasedPlaceholderHeaderRegexStr)
	versionHeaderRegex                           = regexp.MustCompile(versionHeaderRegexStr)
	breakingChangesRegex                         = regexp.MustCompile(breakingChangesSubheaderRegexStr)
	emptyLineRegex                               = regexp.MustCompile("^\\s*$")
	shouldWarnAboutUndoingRemotePushMessage      = `ACTION REQUIRED: An error occurred meaning we need to undo our push to '%s', but this is a dangerous operation for its risk that it will destroy history on the remote so you'll need to do this manually.
	Follow these instructions to properly undo this push:
	1. Run a git fetch to pull down the latest changes from origin main
	2. Verify that the origin main hasn't had any new commits that would get blown away if we reverted it
	3. Ensure that the local branch has cleaned up correctly. Specifically, that it has no leftover changes from running the releaser and is on the correct commit.
	3. Do a 'git push -f %s %s' from local main to remote main
	`
)

var shouldBumpMajorVersion bool
var ReleaseCmd = &cobra.Command{
	Use:   releaseCmdStr,
	Short: "Cuts a new release on the repo",
	Long:  "Cuts a new release on a Kurtosis Repo. This command is intended to be ran in a Github action and requires a release token to authenticate pushes to main.",
	Args:  cobra.ExactArgs(1),
	RunE:  run,
}

var emptyDomain []string = nil

func parseChangeLogFile(changelogFile []byte) (bool, error) {
	tbdHeaderFound := false
	isBreakingChange := false

	foundLastReleasedVersionHeader := false
	foundNonEmptyLineBeforeLastVersionHeader := false
	scanner := bufio.NewScanner(bytes.NewReader(changelogFile))

	for scanner.Scan() {
		// Check if TBD is the first non-empty line - this is for extra caution.
		if !emptyLineRegex.Match(scanner.Bytes()) {
			if !versionToBeReleasedPlaceholderHeaderRegex.Match(scanner.Bytes()) {
				return false, stacktrace.NewError("TBD header is either missing or is not the first non empty line in changelog.md")
			}
			tbdHeaderFound = true
			break
		}
	}

	// No TBD header was found because the file is empty.
	if !tbdHeaderFound {
		return false, stacktrace.NewError("Empty changelog file, please check the filepath again.")
	}

	for scanner.Scan() {
		if versionToBeReleasedPlaceholderHeaderRegex.Match(scanner.Bytes()) {
			return false, stacktrace.NewError("Found more than %d TBD headers, there can only be #d TBD header in the changelog", expectedNumTBDHeaderLines)
		}

		// Scan file until next version header detected, searching for first not empty line along the way
		if versionHeaderRegex.Match(scanner.Bytes()) {
			foundLastReleasedVersionHeader = true
			break
		}

		if !emptyLineRegex.Match(scanner.Bytes()) {
			foundNonEmptyLineBeforeLastVersionHeader = true
		}

		// there exist breaking change header between TBD and last released version
		if breakingChangesRegex.Match(scanner.Bytes()) {
			isBreakingChange = true
		}
	}

	if err := scanner.Err(); err != nil {
		return false, stacktrace.Propagate(err, "An error occurred while scanning the bytes of the changelog file.")
	}

	if !foundLastReleasedVersionHeader {
		return false, stacktrace.NewError("No previous release versions were detected in this changelog. Are you sure that the changelog is in sync with the release tags on this branch?")
	}

	// if first non-empty line after TBD is the version line, it means that changelog.md is empty for upcoming release.
	if !foundNonEmptyLineBeforeLastVersionHeader {
		return false, stacktrace.NewError("changelog.md is empty for the current release, please check if the changes are merged and changelog.md is updated correctly.")
	}

	return isBreakingChange, nil
}

func init() {
	ReleaseCmd.Flags().BoolVarP(&shouldBumpMajorVersion, "bump-major", bumpMajorFlagShortStr, bumpMajorFlagDefaultVal, "If set, in place of doing version autodetection based on the changelog, the major version (\"X\" in X.Y.Z) will be bumped")
}

func run(cmd *cobra.Command, args []string) error {
	logrus.Infof("Setting up authentication using provided token...")
	token := os.Args[2]
	gitAuth := &http.BasicAuth{
		Username: "git", // username doesn't matter
		Password: token,
	}

	logrus.Infof("Starting release process...")
	currentWorkingDirpath, err := os.Getwd()
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred getting the current working directory.")
	}
	gitDirpath := path.Join(currentWorkingDirpath, gitDirname)
	if _, err := os.Stat(gitDirpath); err != nil {
		if os.IsNotExist(err) {
			return stacktrace.Propagate(err, "An error occurred getting the git repository in this directory. This means that this binary is not being run from root of a git repository.")
		}
	}

	logrus.Infof("Retrieving git information...")
	repository, err := git.PlainOpen(currentWorkingDirpath)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while attempting to open the existing git repository.")
	}
	globalRepoConfig, err := repository.ConfigScoped(config.GlobalScope)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while attempting to retrieve the global git config for this repo.")
	}
	name := globalRepoConfig.User.Name
	email := globalRepoConfig.User.Email
	if name == "" || email == "" {
		return stacktrace.NewError("The following empty name or email were detected in global git config'name: %s', 'email: %s'. Make sure these are set for annotating release commits.", name, email)
	}
	originRemote, err := repository.Remote(originRemoteName)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred getting remote '%v' for repository; is the code pushed?", originRemoteName)
	}

	logrus.Infof("Conducting pre release checks...")
	worktree, err := repository.Worktree()
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while trying to retrieve the worktree of the repository.")
	}

	// Check no staged or unstaged changes exist on the branch before release
	currWorktreeStatus, err := worktree.Status()
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while trying to retrieve the status of the worktree of the repository.")
	}
	isClean := currWorktreeStatus.IsClean()
	if !isClean {
		return stacktrace.NewError("The branch contains modified files. Please ensure the working tree is clean before attempting to release. Currently the status is '%s'\n", currWorktreeStatus.String())
	}

	logrus.Infof("Fetching origin if needed...")
	// Fetch remote if needed
	lastFetchedFilepath := path.Join(gitDirpath, lastFetchedFilename)
	shouldFetch, err := determineShouldFetch(lastFetchedFilepath)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while determining if we should fetch from '%s'", lastFetchedFilepath)
	}
	if shouldFetch {
		fetchOpts := &git.FetchOptions{RemoteName: originRemoteName, Auth: gitAuth}
		if err := originRemote.Fetch(fetchOpts); err != nil && err != git.NoErrAlreadyUpToDate {
			return stacktrace.Propagate(err, "An error occurred fetching from the remote repository.")
		}
		currentUnixTimeStr := fmt.Sprint(time.Now().Unix())
		if err := os.WriteFile(lastFetchedFilepath, []byte(currentUnixTimeStr), lastFetchedFileMode); err != nil {
			return stacktrace.Propagate(err, "An error occurred writing last-fetched timestamp '%v' to file '%v'", currentUnixTimeStr, lastFetchedFilepath)
		}
	}

	logrus.Infof("Checking that %s and %s are in sync...", mainBranchName, originRemoteName)
	// Check that local main and remote main are in sync
	localMainBranchName := mainBranchName
	remoteMainBranchName := fmt.Sprintf("%v/%v", originRemoteName, mainBranchName)
	localMainHash, err := repository.ResolveRevision(plumbing.Revision(localMainBranchName))
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred parsing revision '%v'", localMainBranchName)
	}
	remoteMainHash, err := repository.ResolveRevision(plumbing.Revision(remoteMainBranchName))
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred parsing revision '%v'", remoteMainBranchName)
	}
	isLocalMainInSyncWithRemoteMain := localMainHash.String() == remoteMainHash.String()
	if !isLocalMainInSyncWithRemoteMain {
		return stacktrace.NewError("The local '%s' branch is not in sync with the '%s' '%s' branch. Must be in sync to conduct release process.", mainBranchName, originRemoteName, mainBranchName)
	}

	logrus.Infof("Checking out %s branch...", mainBranchName)
	mainBranchRef := plumbing.ReferenceName(fmt.Sprintf("%s%s", headRef, mainBranchName))
	err = worktree.Checkout(&git.CheckoutOptions{Branch: mainBranchRef})
	if err != nil {
		return stacktrace.Propagate(err, "Missing required '%v' branch locally. Please run 'git checkout %v'", mainBranchName, mainBranchName)
	}

	// Conduct changelog file validation
	changelogFilepath := path.Join(currentWorkingDirpath, relChangelogFilepath)
	changelogFile, err := os.ReadFile(changelogFilepath)

	if err != nil {
		return stacktrace.Propagate(err, "An error occurred attempting to read changelog file at provided path. Are you sure '%s' exists?", changelogFilepath)
	}

	hasBreakingChange, err := parseChangeLogFile(changelogFile)

	if err != nil {
		return err
	}

	logrus.Infof("Finished prererelease checks.")

	logrus.Infof("Guessing next release version...")
	latestReleaseVersion, err := getLatestReleaseVersion(repository)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred getting the latest release version.")
	}
	var nextReleaseVersion semver.Version
	if shouldBumpMajorVersion {
		nextReleaseVersion = latestReleaseVersion.IncMajor()
	} else {
		if hasBreakingChange {
			nextReleaseVersion = latestReleaseVersion.IncMinor()
		} else {
			nextReleaseVersion = latestReleaseVersion.IncPatch()
		}
	}

	logrus.Infof("VERIFICATION: Release new version '%s'? (ENTER to continue, Ctrl-C to quit)", nextReleaseVersion.String())
	_, err = fmt.Scanln()
	if err != nil {
		return nil
	}

	shouldResetLocalBranch := true
	defer func() {
		if shouldResetLocalBranch {
			// git reset --hard origin/main
			err = worktree.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: *remoteMainHash})
			if err != nil {
				logrus.Errorf("ACTION REQUIRED: Error occurred attempting to undo local changes made for release '%s'. Please run 'git reset --hard %s' to undo manually.", nextReleaseVersion.String(), remoteMainBranchName)
			}
		}
	}()

	logrus.Infof("Running prerelease scripts...")
	err = runPreReleaseScripts(currentWorkingDirpath, nextReleaseVersion.String())
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while running prerelease scripts.")
	}

	logrus.Infof("Updating the changelog...")
	err = updateChangelog(changelogFilepath, nextReleaseVersion.String())
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while updating the changelog file at '%s'", changelogFilepath)
	}

	// we have to manually populate the excludes because of https://github.com/kurtosis-tech/kudet/issues/22
	// we should remove this piece when the above issue & bigger go-git issue gets resolved
	logrus.Infof("Populating excludes for the worktree by parsing the .gitignore file")
	gitIgnoreFile, err := os.Open(gitIgnoreRelFilepath)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while reading the '%v' file", gitIgnoreRelFilepath)
	}
	defer gitIgnoreFile.Close()

	gitIgnoreFileScanner := bufio.NewScanner(gitIgnoreFile)
	// split the file by lines
	gitIgnoreFileScanner.Split(bufio.ScanLines)
	for gitIgnoreFileScanner.Scan() {
		pattern := gitIgnoreFileScanner.Text()
		if isWhiteSpaceOrComment(pattern) {
			continue
		}
		worktree.Excludes = append(worktree.Excludes, gitignore.ParsePattern(pattern, emptyDomain))
	}

	logrus.Infof("Committing changes locally...")
	err = worktree.AddWithOptions(&git.AddOptions{All: true})
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while adding files to the staging area")
	}

	commitMsg := fmt.Sprintf("Finalize changes for release version '%s'", nextReleaseVersion.String())
	_, err = worktree.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  name,
			Email: email,
			When:  time.Now(),
		},
	})

	logrus.Infof("Setting next release version tag...")
	// Set next release version tag
	releaseTag := nextReleaseVersion.String()
	vReleaseTag := fmt.Sprintf("v%s", nextReleaseVersion.String())
	head, err := repository.Head()
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while attempting to get the ref to HEAD of the local repository.")
	}
	_, err = repository.CreateTag(releaseTag, head.Hash(), &git.CreateTagOptions{
		Message: releaseTag,
	})
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while attempting to create this git tag for the next release version '%s'", releaseTag)
	}
	shouldDeleteLocalReleaseTag := true
	defer func() {
		if shouldDeleteLocalReleaseTag {
			// git tag -d
			err = repository.DeleteTag(releaseTag)
			if err != nil {
				logrus.Errorf("ACTION REQUIRED: An error occurred attempting to undo creation of tag '%s'. Please run 'git tag -d %s' to delete the tag manually.", releaseTag, err)
			}
		}
	}()
	_, err = repository.CreateTag(vReleaseTag, head.Hash(), &git.CreateTagOptions{
		Message: vReleaseTag,
	})
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while attempting to create this git tag for the next release version '%s'", vReleaseTag)
	}
	shouldDeleteLocalVPrefixedReleaseTag := true
	defer func() {
		if shouldDeleteLocalVPrefixedReleaseTag {
			// git tag -d
			err = repository.DeleteTag(vReleaseTag)
			if err != nil {
				logrus.Errorf("ACTION REQUIRED: An error occurred attempting to undo creation of tag '%s'. Please run 'git tag -d %s' to delete the tag manually.", vReleaseTag, vReleaseTag)
			}
		}
	}()

	// The order in which we push resources to remote is: vReleaseTag -> Commits -> Release Tag
	// This is important because we push in order of easiest to reverse to harder to reverse in case of failures
	// With pushing Release Tag to remote being the point at which operations are irreversible due to CI being triggered

	vReleaseTagRefSpec := fmt.Sprintf("refs/tags/%s:refs/tags/%s", vReleaseTag, vReleaseTag)
	pushVPrefixedReleaseTagOpts := &git.PushOptions{
		RemoteName: originRemoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec(vReleaseTagRefSpec)},
		Auth:       gitAuth,
	}
	if err = repository.Push(pushVPrefixedReleaseTagOpts); err != nil {
		logrus.Errorf("An error occurred while pushing release tag: '%s' to '%s'.", vReleaseTag, remoteMainBranchName)
	}
	shouldDeleteRemoteVPrefixedReleaseTag := true
	defer func() {
		if shouldDeleteRemoteVPrefixedReleaseTag {
			// git push origin :tagname
			emptyVReleaseTagRefSpec := fmt.Sprintf(":refs/tags/%s", vReleaseTag)
			deleteVPrefixedReleaseTagPushOpts := &git.PushOptions{
				RemoteName: originRemoteName,
				RefSpecs:   []config.RefSpec{config.RefSpec(emptyVReleaseTagRefSpec)},
				Auth:       gitAuth,
			}
			err = repository.Push(deleteVPrefixedReleaseTagPushOpts)
			if err != nil {
				logrus.Errorf("ACTION REQUIRED: An error occurred attempting to delete tag '%s' from '%s'. Please run 'git push --delete %s %s' to delete the tag manually.", vReleaseTag, originRemoteName, originRemoteName, vReleaseTag)
			}
		}
	}()

	logrus.Infof("Pushing release changes to '%s'...", remoteMainBranchName)
	pushCommitOpts := &git.PushOptions{RemoteName: originRemoteName, Auth: gitAuth}
	if err = repository.Push(pushCommitOpts); err != nil {
		return stacktrace.Propagate(err, "An error occurred while pushing release changes to '%s'", remoteMainBranchName)
	}
	shouldWarnAboutUndoingRemotePush := true
	defer func() {
		if shouldWarnAboutUndoingRemotePush {
			logrus.Errorf(shouldWarnAboutUndoingRemotePushMessage, originRemoteName, originRemoteName, mainBranchName, err)
		}
	}()

	logrus.Infof("Pushing release tags to '%s'...", remoteMainBranchName)
	releaseTagRefSpec := fmt.Sprintf("refs/tags/%s:refs/tags/%s", releaseTag, releaseTag)
	pushReleaseTagOpts := &git.PushOptions{
		RemoteName: originRemoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec(releaseTagRefSpec)},
		Auth:       gitAuth,
	}
	if err = repository.Push(pushReleaseTagOpts); err != nil {
		return stacktrace.Propagate(err, "An error occurred while pushing release tag: '%s' to '%s'", releaseTag, remoteMainBranchName)
	}

	shouldResetLocalBranch = false
	shouldDeleteLocalReleaseTag = false
	shouldDeleteLocalVPrefixedReleaseTag = false
	shouldDeleteRemoteVPrefixedReleaseTag = false
	shouldWarnAboutUndoingRemotePush = false

	logrus.Infof("Release success.")
	return nil
}

// ====================================================================================================
//
//	Private Helper Functions
//
// ====================================================================================================
func determineShouldFetch(lastFetchedFilepath string) (bool, error) {
	lastFetchedUnixTimeStr, err := os.ReadFile(lastFetchedFilepath)
	if err != nil {
		if os.IsNotExist(err) {
			logrus.Infof("An error occurred opening the file containing the last-fetched timestamp at '%s'", lastFetchedFilepath)
			return true, nil
		}
		return false, stacktrace.Propagate(err, "An error occurred reading the file to determine fetching '%s'", lastFetchedFilepath)
	}

	lastFetchedUnixTime, err := strconv.ParseUint(
		string(lastFetchedUnixTimeStr),
		lastFetchedTimestampUintParseBase,
		lastFetchedTimestampUintParseBits,
	)
	if err != nil {
		return false, stacktrace.Propagate(err, "An error occurred parsing last-fetch Unix time string '%v'", lastFetchedUnixTimeStr)
	}
	lastFetchedTime := time.Unix(int64(lastFetchedUnixTime), extraNanosecondsToAddToLastFetchedTimestamp)
	noFetchNeededBefore := lastFetchedTime.Add(fetchGracePeriod)

	return time.Now().After(noFetchNeededBefore), nil
}

func getLatestReleaseVersion(repo *git.Repository) (*semver.Version, error) {
	tagrefs, err := repo.Tags()
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred while retrieving tags for repository.")
	}

	// Trim tagrefs and filter for only tags with X.Y.Z version format
	var allTagSemVers []*semver.Version
	err = tagrefs.ForEach(func(tagref *plumbing.Reference) error {
		tagName := tagref.Name().String()
		tagName = strings.ReplaceAll(tagName, tagsPrefix, "")

		if semverRegex.Match([]byte(tagName)) {
			tagSemVer, err := semver.StrictNewVersion(tagName)
			if err != nil {
				return stacktrace.Propagate(err, "An error occurred parsing '%s' tag into a semver object.", tagName)
			}
			allTagSemVers = append(allTagSemVers, tagSemVer)
		}
		return nil
	})
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred while iterating through tagrefs in the repository.")
	}

	var latestReleaseTagSemVer *semver.Version
	if len(allTagSemVers) == 0 {
		latestReleaseTagSemVer, err = semver.StrictNewVersion(noPreviousVersion)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred creating '%s' semantic version.", noPreviousVersion)
		}
	} else {
		sort.Sort(sort.Reverse(semver.Collection(allTagSemVers)))
		latestReleaseTagSemVer = allTagSemVers[0]
	}

	return latestReleaseTagSemVer, nil
}

func runPreReleaseScripts(preReleaseScriptsDirpath string, releaseVersion string) error {
	preReleaseScriptsFilepath := path.Join(preReleaseScriptsDirpath, preReleaseScriptsFilename)
	preReleaseScriptsFile, err := os.ReadFile(preReleaseScriptsFilepath)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred attempting to open file at provided path. Are you sure '%s' exists?", preReleaseScriptsFilepath)
	}

	lines := bytes.Split(preReleaseScriptsFile, []byte("\n"))
	for _, line := range lines {
		scriptFilepath := string(line)
		if strings.TrimSpace(scriptFilepath) == "" {
			continue
		}
		scriptCmdString := path.Join(preReleaseScriptsDirpath, scriptFilepath)
		scriptCmd := exec.Command(scriptCmdString, releaseVersion)

		if err := scriptCmd.Run(); err != nil {
			castedErr, ok := err.(*exec.ExitError)
			if !ok {
				return stacktrace.Propagate(err, "Pre release script command '%s %s' failed with an unrecognized error", scriptCmdString, releaseVersion)
			}
			return stacktrace.NewError("Pre release script command '%s %s' returned logs:\n%s", scriptCmdString, releaseVersion, string(castedErr.Stderr))
		}
	}

	return nil
}

func updateChangelog(changelogFilepath string, releaseVersion string) error {
	changelogFile, err := os.ReadFile(changelogFilepath)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred attempting to open changelog file at provided path. Are you sure '%s' exists?", changelogFilepath)
	}
	lines := bytes.Split(changelogFile, []byte("\n"))
	emptyLine := []byte("\n")

	// Check that first line contains version to be released placeholder header
	if !versionToBeReleasedPlaceholderHeaderRegex.Match(lines[0]) {
		return stacktrace.NewError("No '%s' found in the first line of the changelog. Check the changelog at '%s' is in the correct format.", versionToBeReleasedPlaceholderHeaderStr, changelogFilepath)
	}
	// Create new update changelog file
	updatedChangelogFile, err := os.Create(changelogFilepath)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred attempting to create the updated changelog file at '%s'", changelogFilepath)
	}
	// Write version to be released placeholder header as the first line
	_, err = updatedChangelogFile.Write([]byte(string(lines[0]) + "\n"))
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred attempting to write '%s' to the updated changelog file at '%s'", versionToBeReleasedPlaceholderHeaderStr, changelogFilepath)
	}
	// Write an empty line
	_, err = updatedChangelogFile.Write(emptyLine)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred attempting to write empty line to the updated changelog file at '%s'", changelogFilepath)
	}
	// Write the new version header
	releaseVersionHeader := fmt.Sprintf("%s %s", sectionHeaderPrefix, releaseVersion)
	_, err = updatedChangelogFile.Write([]byte(releaseVersionHeader))
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred attempting to write '%s' to the updated changelog file at '%s'", versionToBeReleasedPlaceholderHeaderStr, changelogFilepath)
	}
	// Write another empty line
	_, err = updatedChangelogFile.Write(emptyLine)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred attempting to write an empty line after the new version header to the updated changelog file at '%s'", changelogFilepath)
	}
	// Write the rest of the lines
	_, err = updatedChangelogFile.Write(bytes.Join(lines[1:], []byte("\n")))
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred attempting to append existing the existing changelog file contents to the updated changelog file at '%s':\n", changelogFilepath)
	}

	return nil
}

func isWhiteSpaceOrComment(pattern string) bool {
	if strings.HasPrefix(pattern, gitIgnoreCommentCharacter) {
		return true
	}
	return strings.TrimSpace(pattern) == ""
}
