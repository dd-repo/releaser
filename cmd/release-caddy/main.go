package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/alecaivazis/survey"
	"github.com/caddyserver/buildworker"
	"github.com/google/go-github/github"
)

var (
	caddyRepo = filepath.Join(os.Getenv("GOPATH"), "src", buildworker.CaddyPackage)

	githubAccessToken = os.Getenv("GITHUB_TOKEN")

	devportalAccountID = os.Getenv("DEVPORTAL_ID")  // account ID at caddyserver.com
	devportalAPIKey    = os.Getenv("DEVPORTAL_KEY") // associated API key

	// resume allows us to skip some deploy steps using the most recent, existing tag.
	// only use resume if a tag was pushed but a subsequent step failed.
	resume string
)

const (
	githubOwner = "mholt" // the owner of the repository to publish to
	githubRepo  = "caddy" // the owner's repository to publish to
	websiteURL  = "http://localhost:2015" // URL to the Caddy website
)

func main() {
	flag.StringVar(&resume, "resume", "", `may be "github" to skip all deploy steps and resume most recent deploy if failed`)
	flag.Parse()

	fmt.Printf("Using Caddy source at: %s\n", caddyRepo)

	// some initial checks before we begin
	if err := envVariablesSet(); err != nil {
		log.Fatalf("Aborting deployment: %v", err)
	}
	if err := workingCopyClean(); err != nil {
		log.Fatalf("Aborting deployment: %v", err)
	}

	var tag string
	var prerelease bool
	var err error

	// see if we're resuming a deploy; only do this if a
	// tag was pushed but some step after the push failed.
	if resume != "" {
		// resume a deploy

		tag, err = getCurrentTag()
		if err != nil {
			log.Fatal(err)
		}
		prerelease = isPrerelease(tag)

		if resume == "github" {
			fmt.Printf("\nNOTE: The deploy for %s is being resumed.\n", tag)
			fmt.Println("The process will pick up at publishing a release on GitHub.")
		} else {
			log.Fatal("Unknown resume state")
		}

		confirmed, err := askYesNo("Continue?")
		if err != nil {
			log.Fatal(err)
		}
		if !confirmed {
			log.Fatal("Aborting resumed deployment")
		}
	} else {
		// begin a new deploy

		if err := confirmRightCommit(); err != nil {
			log.Fatalf("Aborting deployment: %v", err)
		}
		if err := confirmReadmeUpdated(); err != nil {
			log.Fatalf("Aborting deployment: %v", err)
		}

		// get the tag for the new release
		tag, prerelease, err = askNewTagVersion()
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
	}

	// here we goooo!
	err = deploy(tag, prerelease, resume)
	if err != nil {
		fmt.Print("\a") // terminal bell, since we might be minutes into a deploy
		log.Fatal(err)
	}

	log.Println("Done.")
	log.Printf("%s release successful.", tag)
}

// deploy runs checks on caddy, and if they succeed, tags
// the current commit and releases Caddy. Pass in the name
// of the tag, whether it is a pre-release, and where to
// resume the deploy at, if at all (otherwise empty string).
func deploy(tag string, prerelease bool, resume string) error {
	if resume == "" {
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

		// Wait a moment before publishing the release; I've seen the API call
		// to publish a release on GitHub fail with "Published releases must
		// have a valid tag" even after pushing the tag. I suspect that their
		// system must be only "eventually consistent" so perhaps by waiting a
		// few seconds, we'll alleviate any sort of race condition they have.
		log.Println("Waiting a few seconds before publishing release...")
		time.Sleep(5 * time.Second)
	}

	// create release on GitHub
	log.Println("Publishing release to GitHub")
	ghClient, release, err := publishReleaseToGitHub(tag, prerelease)
	if err != nil {
		return fmt.Errorf("creating release: %v", err)
	}

	// set up environment in which to perform builds
	log.Println("Preparing builds")
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
				log.Printf("building %s: %v\n", plat, err)
				log.Printf(">>>>>>>>>>>>%s\n<<<<<<<<<<<<\n", deployEnv.Log.String())
				return
			}
			defer func() {
				file.Close()
				os.Remove(file.Name())
			}()

			// TODO: upload a text file with the SHA-256 of all
			// release assets uploaded to GitHub.

			// upload
			uploadThrottle <- struct{}{}
			defer func() { <-uploadThrottle }()
			log.Printf("Uploading %s...", plat)
			_, _, err = ghClient.Repositories.UploadReleaseAsset(context.Background(), githubOwner,
				githubRepo, release.GetID(), &github.UploadOptions{Name: filepath.Base(file.Name())}, file)
			if err != nil {
				log.Printf("!! Error uploading %+v: %v", plat, err)
				return
			}
			log.Printf("Uploaded %s successfully", plat)
		}(tag, plat)
	}

	wg.Wait()

	// deploy to Caddy build server if not a pre-release
	if !prerelease {
		log.Println("Deploying to build server")

		// prepare request body
		type DeployRequest struct {
			CaddyVersion string `json:"caddy_version"`
		}
		bodyInfo := DeployRequest{CaddyVersion: tag}
		body, err := json.Marshal(bodyInfo)
		if err != nil {
			return fmt.Errorf("preparing request body: %v", err)
		}

		// prepare request
		req, err := http.NewRequest("POST", websiteURL+"/api/deploy-caddy", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("preparing request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(devportalAccountID, devportalAPIKey)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("network error deploying to website: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			respBody, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("reading response body: %v", err)
			}
			return fmt.Errorf("deploy to build server failed, HTTP %d: %s", resp.StatusCode, respBody)
		}

		log.Printf("Deploy request successfully sent to Caddy build server")
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
	err = be.RunCaddyChecks()
	if err != nil {
		log.Printf("error; here's the log:\n>>>>>>>>>>>>%s\n<<<<<<<<<<<<\n", be.Log.String())
	}
	return err
}

// publishReleaseToGitHub makes a new release on GitHub
// and returns the client, the release, and an error if any.
func publishReleaseToGitHub(tag string, prerelease bool) (*github.Client, *github.RepositoryRelease, error) {
	tc := oauth2.NewClient(oauth2.NoContext, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: githubAccessToken},
	))
	client := github.NewClient(tc)
	release, _, err := client.Repositories.CreateRelease(context.Background(), githubOwner, githubRepo,
		&github.RepositoryRelease{
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
	if devportalAccountID == "" {
		return fmt.Errorf("environment variable DEVPORTAL_ID cannot be empty")
	}
	if devportalAPIKey == "" {
		return fmt.Errorf("environment variable DEVPORTAL_KEY cannot be empty")
	}
	if os.Getenv("GOPATH") == "" {
		return fmt.Errorf("environment variable GOPATH cannot be empty")
	}
	return nil
}

// workingCopyClean asserts that the caddy repository has
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

// getCurrentTag returns the current tag of the Caddy repo.
// If there is no current tag, a "dummy" tag of "v0.0.0" will
// be returned for consistency with semantic versioning.
func getCurrentTag() (string, error) {
	cmd := exec.Command("git", "tag")
	cmd.Dir = caddyRepo
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	allTags := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(allTags) == 0 || (len(allTags) == 1 && allTags[0] == "") {
		allTags = []string{"v0.0.0"} // alright--starting from nothing, are we?
	}

	// sort by comparing each version label successively; string
	// sort won't do the trick because 10 < 9 sorting by strings.
	sort.Slice(allTags, func(i int, j int) bool {
		tagI := strings.TrimLeft("v", allTags[i])
		tagJ := strings.TrimLeft("v", allTags[j])
		partsI := strings.Split(tagI, ".")
		partsJ := strings.Split(tagJ, ".")
		for len(partsI) < 3 {
			partsI = append(partsI, "0")
		}
		for len(partsJ) < 3 {
			partsJ = append(partsJ, "0")
		}
		for k := 0; k < 3; k++ {
			if partsI[k] == partsJ[k] {
				continue
			}
			numI, err := strconv.Atoi(partsI[k])
			if err != nil {
				return false
			}
			numJ, err := strconv.Atoi(partsI[j])
			if err != nil {
				return false
			}
			return numI < numJ
		}
		return false
	})

	// return the first tag, which is the "highest" (most recent) version
	return allTags[0], nil
}

// isPrerelease returns true if tag looks like a pre-release version.
func isPrerelease(tag string) bool {
	return strings.Contains(tag, "-alpha") ||
		strings.Contains(tag, "-beta") ||
		strings.Contains(tag, "-pre") ||
		strings.Contains(tag, "-rc")
}

// nextTagSuggestions returns a list of suggested tags based on the
// most recent tag, which must be passed in as currentTagRaw.
func nextTagSuggestions(currentTagRaw string) ([]string, error) {
	currentTag := strings.TrimLeft(currentTagRaw, "v")
	tagParts := strings.Split(currentTag, ".")
	for len(tagParts) < 3 {
		tagParts = append(tagParts, "0")
	}

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
		if len(nextVer) == 3 && nextVer[2] == "0" {
			nextVer = nextVer[:2] // drop trailing ".0" in third part ("v0.10" instead of "v0.10.0")
		}
		next := strings.Join(nextVer, ".")
		if strings.HasPrefix(currentTagRaw, "v") {
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
	currentTagRaw, err := getCurrentTag()
	if err != nil {
		return "", false, err
	}

	nextVers, err := nextTagSuggestions(currentTagRaw)
	if err != nil {
		return "", false, err
	}

	const other = "Other..."
	tag, err := survey.AskOneValidate(&survey.Choice{
		Message: "Current tag is " + currentTagRaw + ". What should the new tag be?",
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

	return tag, isPrerelease(tag), nil
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
