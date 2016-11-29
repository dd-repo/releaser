package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/oauth2"

	"github.com/alecaivazis/survey"
	"github.com/caddyserver/buildworker"
	"github.com/google/go-github/github"
)

var (
	caddyRepo = filepath.Join(os.Getenv("GOPATH"), "src", buildworker.CaddyPackage)

	githubAccessToken = os.Getenv("GITHUB_TOKEN")
	buildServerID     = os.Getenv("BUILD_SERVER_ID")
	buildServerKey    = os.Getenv("BUILD_SERVER_KEY")
)

const (
	githubOwner = "mholt" // the owner of the repository to publish to
	githubRepo  = "caddy" // the owner's repository to publish to
)

func main() {
	fmt.Printf("Using Caddy source at: %s\n", caddyRepo)

	// some initial checks before we begin
	if err := envVariablesSet(); err != nil {
		log.Fatalf("Aborting deployment: %v", err)
	}
	if err := workingCopyClean(); err != nil {
		log.Fatalf("Aborting deployment: %v", err)
	}
	if err := confirmRightCommit(); err != nil {
		log.Fatalf("Aborting deployment: %v", err)
	}
	if err := confirmReadmeUpdated(); err != nil {
		log.Fatalf("Aborting deployment: %v", err)
	}

	// get the tag for the new release
	tag, prerelease, err := askNewTagVersion()
	if err != nil {
		log.Fatal(err)
	}

	// one more check
	fmt.Println("\nNOTICE: If you continue, your GOPATH will be updated")
	fmt.Printf("by running `go get -u %s` \n", buildworker.CaddyPackage)
	fmt.Println("before checks are performed. Tests will follow, and")
	fmt.Println("the release will continue only if the tests pass.")
	confirmed, err := askYesNo("I'm ready. Are you ready? There's no going back:")
	if err != nil {
		log.Fatal(err)
	}
	if !confirmed {
		log.Fatal("Aborting deployment: operator not ready 🙄")
	}

	// here we goooo!
	err = deploy(tag, prerelease)
	if err != nil {
		fmt.Print("\a") // terminal bell, since we might be minutes into a deploy
		log.Fatal(err)
	}

	log.Println("Done.")
	log.Printf("%s release successful.", tag)
}

// deploy runs checks on caddy, and if they succeed, tags
// the current commit and releases Caddy. Pass in the name
// of the tag and whether it is a pre-release.
func deploy(tag string, prerelease bool) error {
	// run checks to make sure it, you know, works.
	err := checkCaddy()
	if err != nil {
		return fmt.Errorf("checks: %v", err)
	}

	// git tag (signed)
	err = run("git", "tag", "-s", tag, "-m", "")
	if err != nil {
		return fmt.Errorf("creating signed tag: %v", err)
	}

	// git push
	err = run("git", "push")
	if err != nil {
		return fmt.Errorf("git push: %v", err)
	}

	// git push tag
	err = run("git", "push", "--tags")
	if err != nil {
		return fmt.Errorf("pushing tag: %v", err)
	}

	// create release on GitHub
	ghClient, release, err := publishReleaseToGitHub(tag, prerelease)
	if err != nil {
		return fmt.Errorf("creating release: %v", err)
	}

	// set up environment in which to perform builds
	deployEnv, err := buildworker.Open(tag, nil)
	if err != nil {
		return fmt.Errorf("opening build environment: %v", err)
	}
	defer deployEnv.Close()

	// the demand for Caddy on these platforms is very low
	// and the demand on the CPU is very high
	skip := append(buildworker.UnsupportedPlatforms, []buildworker.Platform{
		{OS: "dragonfly"},
		{OS: "solaris"},
		{OS: "netbsd"},
		{ARM: "5"},
		{ARM: "6"},
		{OS: "darwin", Arch: "386"},
		{OS: "darwin", Arch: "arm64"},
		{Arch: "mips64"},
		{Arch: "mips64le"},
		{Arch: "ppc64"},
		{Arch: "ppc64le"},
		{OS: "openbsd", Arch: "386"},
		{OS: "openbsd", Arch: "arm"},
		{OS: "freebsd", Arch: "386"},
		{OS: "freebsd", Arch: "arm"},
	}...)

	platforms, err := buildworker.SupportedPlatforms(skip)
	if err != nil {
		return err
	}

	// make a temporary folder where we will store build assets while
	// they upload; the name of each asset will be unique by platform.
	tmpdir, err := ioutil.TempDir("", "caddy_deployment_")
	if err != nil {
		return fmt.Errorf("making temporary directory: %v", err)
	}
	defer os.RemoveAll(tmpdir)

	// perform some number of builds concurrently; throttle uploads separately
	var wg sync.WaitGroup
	var buildThrottle, uploadThrottle = make(chan struct{}, 2), make(chan struct{}, 3)

	// build and upload a static release for each platform we choose
	for _, plat := range platforms {
		wg.Add(1)
		buildThrottle <- struct{}{}

		go func(tag string, plat buildworker.Platform) {
			defer wg.Done()

			// build
			log.Printf("Building %s...", plat)
			file, err := deployEnv.Build(plat, tmpdir)
			<-buildThrottle
			if err != nil {
				log.Printf("building %s: %v", plat, err)
				return
			}
			defer func() {
				file.Close()
				os.Remove(file.Name())
			}()

			// upload
			uploadThrottle <- struct{}{}
			defer func() { <-uploadThrottle }()
			log.Printf("Uploading %s...", plat)
			_, _, err = ghClient.Repositories.UploadReleaseAsset(githubOwner, githubRepo, *release.ID, &github.UploadOptions{
				Name: filepath.Base(file.Name()),
			}, file)
			if err != nil {
				log.Printf("!! Error uploading %+v: %v", plat, err)
				return
			}
			log.Printf("Uploaded %s successfully", plat)
		}(tag, plat)
	}

	wg.Wait()

	if !prerelease {
		// TODO: Deploy to Caddy build server
	}

	return nil
}

func checkCaddy() error {
	// get current commit
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = caddyRepo
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	currentCommit := strings.TrimSpace(string(out))

	// create build environment, with no plugins
	be, err := buildworker.Open(currentCommit, nil)
	if err != nil {
		return fmt.Errorf("opening build environment: %v", err)
	}
	defer be.Close()

	// update master GOPATH to help ensure the tests
	// here will get the same results as the build
	// server which also updates its GOPATH each time
	// a deploy is made; it's not bulletproof but it's
	// good enough. if this update introduces some
	// breaking change, the tests we're about to run
	// will catch that -- however, we don't revert
	// the update, as that would involve a massive
	// overwrite of the whole GOPATH on some developer's
	// machine, which makes me uncomfortable.
	err = be.UpdateMasterGopath()
	if err != nil {
		return fmt.Errorf("updating master GOPATH: %v", err)
	}

	// run checks and report results
	return be.RunCaddyChecks()
}

// publishReleaseToGitHub makes a new release on GitHub
// and returns the client, the release, and an error if any.
func publishReleaseToGitHub(tag string, prerelease bool) (*github.Client, *github.RepositoryRelease, error) {
	tc := oauth2.NewClient(oauth2.NoContext, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: githubAccessToken},
	))
	client := github.NewClient(tc)
	release, _, err := client.Repositories.CreateRelease(githubOwner, githubRepo, &github.RepositoryRelease{
		TagName:    github.String(tag),
		Name:       github.String(strings.TrimPrefix(tag, "v")),
		Prerelease: github.Bool(prerelease),
	})
	return client, release, err
}

// envVariablesSet asserts that required environment variables
// are set. Returns an error if a value is missing.
func envVariablesSet() error {
	if githubAccessToken == "" {
		return fmt.Errorf("environment variable GITHUB_TOKEN cannot be empty")
	}
	if buildServerKey == "" {
		return fmt.Errorf("environment variable BUILD_SERVER_ID cannot be empty")
	}
	if buildServerKey == "" {
		return fmt.Errorf("environment variable BUILD_SERVER_KEY cannot be empty")
	}
	if os.Getenv("GOPATH") == "" {
		return fmt.Errorf("environment variable GOPATH cannot be empty")
	}
	return nil
}

// workingCopyClean asserts that thecaddy repository has
// no uncommitted changes. If an error is returned, then
// either an error occurred, or `git status` showed that
// tracked files have been modified. It is not advisable
// to build Caddy in this state since it would lead to
// "unclean" version information; deploys should be done
// exactly on tags and without modifications.
func workingCopyClean() error {
	cmd := exec.Command("git", "status", "--untracked-files=no", "--porcelain")
	cmd.Dir = caddyRepo
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("uncommitted changes; working tree must be clean to deploy")
	}
	return nil
}

// confirmRightCommit asks the operator to confirm that the
// current commit is the right one at which to tag and deploy.
// Returns an error if it isn't.
func confirmRightCommit() error {
	fmt.Printf("Caddy will be deployed at the current commit:\n\n")

	cmd := exec.Command("git", "show", "--summary")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = caddyRepo
	cmd.Run()
	fmt.Printf("\n")

	confirmed, err := askYesNo("Is this the right commit to release?")
	if err != nil {
		return err
	}
	if !confirmed {
		return fmt.Errorf("deploy cancelled by user")
	}

	return nil
}

// confirmReadmeUpdated asks if the readme has been updated.
// Returns an error if it hasn't.
func confirmReadmeUpdated() error {
	confirmed, err := askYesNo("Have README.txt and CHANGES.txt been updated for the new version?")
	if err != nil {
		return err
	}
	if !confirmed {
		return fmt.Errorf("deploy cancelled by user")
	}
	return nil
}

// nextTagSuggestions returns a list of suggested
// tags based on the most recent tag.
func nextTagSuggestions() ([]string, error) {
	cmd := exec.Command("git", "tag")
	cmd.Dir = caddyRepo
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	allTags := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(allTags) == 0 || (len(allTags) == 1 && allTags[0] == "") {
		allTags = []string{"v0.0.0"} // alright--starting from nothing, are we?
	}
	sort.Strings(allTags)

	// get the last tag in a nicely-parsed state
	lastTagRaw := allTags[len(allTags)-1]
	lastTag := strings.TrimLeft(lastTagRaw, "v")
	tagParts := strings.Split(lastTag, ".")

	// viable tags come from incrementing each part
	// of the semantic version number, and setting
	// subsequent parts to 0.
	var nextVers []string
	for i := len(tagParts) - 1; i >= 0; i-- {
		num, err := strconv.Atoi(tagParts[i])
		if err != nil {
			continue
		}
		nextVer := make([]string, len(tagParts))
		copy(nextVer, tagParts)
		nextVer[i] = strconv.Itoa(num + 1)
		for j := i + 1; j < len(nextVer); j++ {
			nextVer[j] = "0"
		}
		next := strings.Join(nextVer, ".")
		if strings.HasPrefix(lastTagRaw, "v") {
			next = "v" + next
		}
		nextVers = append(nextVers, next)
	}

	return nextVers, nil
}

// askNewTagVersion asks for the name of the tag for
// this release. It returns the tag name, whether
// this is a pre-release tag, and/or an error.
func askNewTagVersion() (string, bool, error) {
	nextVers, err := nextTagSuggestions()
	if err != nil {
		return "", false, err
	}

	const other = "Other..."
	tag, err := survey.AskOneValidate(&survey.Choice{
		Message: "What should the new tag be?",
		Choices: append(nextVers, other),
	}, survey.Required)
	if err != nil {
		return "", false, err
	}

	if tag == other {
		tag, err = survey.AskOneValidate(&survey.Input{
			Message: "Type a name for the new tag:",
		}, survey.Required)
		if err != nil {
			return "", false, err
		}
	}

	prerelease := strings.Contains(tag, "-alpha") ||
		strings.Contains(tag, "-beta") ||
		strings.Contains(tag, "-pre") ||
		strings.Contains(tag, "-rc")

	return tag, prerelease, nil
}

// askYesNo asks a No/Yes question and returns true
// if Yes, false if No.
func askYesNo(question string) (bool, error) {
	yn, err := survey.AskOneValidate(&survey.Choice{
		Message: question,
		Choices: []string{"No", "Yes"},
	}, survey.Required)
	if err != nil {
		return false, err
	}
	return yn == "Yes", nil
}

// run runs command with the given args in the caddy repo.
// It directs stdout and stderr through to the user.
// It does not capture the output.
func run(command string, args ...string) error {
	cmd := exec.Command(command, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = caddyRepo
	return cmd.Run()
}
