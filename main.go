package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	osUser "os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"crypto/tls"

	"code.google.com/p/go-netrc/netrc"
	"code.google.com/p/goauth2/oauth"
	"github.com/daviddengcn/go-colortext"
	"github.com/google/go-github/github"
)

const (
	markUnknown = "?"
	markFailure = "✗"
	markPending = "●"
	markSuccess = "✓"
)

var statusColors = map[string]ct.Color{
	markFailure: ct.Red,
	markPending: ct.Yellow,
	markSuccess: ct.Green,
}

var cacheExpirationSeconds = map[string]int64{
	"":        30,
	"failure": -1, // forever
	"pending": 10,
	"success": -1, // forever
}

func runGit(command ...string) string {
	cmd := exec.Command("git", command...)
	cmd.Stderr = os.Stderr

	buf, err := cmd.Output()
	if err != nil {
		die(fmt.Sprintf("'git %s' failed: %s", strings.Join(command, " "), err))
	}

	return strings.TrimRight(string(buf), "\n")
}

func die(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}

func normalizeURL(urlString string) (*url.URL, error) {
	reScheme := regexp.MustCompile(`^[\w+]+://`)

	urlString = reScheme.ReplaceAllLiteralString(urlString, "https://")
	if strings.HasPrefix(urlString, "https://") == false {
		urlString = "https://" + strings.Replace(urlString, ":", "/", 1)
	}

	return url.Parse(strings.TrimSuffix(urlString, ".git"))
}

func targetRevision(args []string) string {
	rev := "HEAD"
	if len(args) >= 1 {
		rev = args[0]
	}
	rev = runGit("rev-parse", rev)

	return rev
}

func restoreCache() (programCache, *os.File) {
	var cache programCache

	// Open cache file under .github-commit-status dir
	cacheFilePath := filepath.Join(
		runGit("rev-parse", "--show-toplevel"),
		".github-commit-status",
		"cache",
	)
	cacheFile, err := os.OpenFile(cacheFilePath, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		die(err.Error())
	}

	json.NewDecoder(cacheFile).Decode(&cache)

	return cache, cacheFile
}

func storeCache(cache programCache, cacheFile *os.File) {
	cacheFile.Truncate(0)
	cacheFile.Seek(0, os.SEEK_SET)
	err := json.NewEncoder(cacheFile).Encode(&cache)
	if err != nil {
		die(err.Error())
	}
}

func retrieveAPIToken(remoteURL *url.URL) string {
	var token string

	// try environment variable
	token = os.Getenv("GITHUB_COMMIT_STATUS_MARK_TOKEN")

	// ..then .netrc
	if token == "" {
		if user, _ := osUser.Current(); user != nil {
			netrcFile := filepath.Join(user.HomeDir, ".netrc")
			if fi, _ := os.Stat(netrcFile); fi != nil {
				apiHost := remoteURL.Host
				if apiHost == "github.com" {
					apiHost = "api.github.com"
				}

				machine, _ := netrc.FindMachine(netrcFile, apiHost)
				// ignore "default" machine
				if machine != nil && machine.Name != "" {
					token = machine.Password
				}
			}
		}
	}

	// ..then git config
	if token == "" {
		token = runGit("config", "--get-urlmatch", "github.token", remoteURL.String())
	}

	return token
}

type programCacheStatus struct {
	Status       string
	LastModified int64
}

type programCache struct {
	Revisions map[string]programCacheStatus
}

func main() {
	var (
		useCache    = flag.Bool("cached", false, "Output cached status")
		updateCache = flag.Bool("update", false, "Force fetch status")
	)
	flag.Parse()

	rev := targetRevision(flag.Args())

	cache, cacheFile := restoreCache()

	cachedStatus := cache.Revisions[rev]

	if *updateCache {
		*useCache = false
	} else {
		expSecs := cacheExpirationSeconds[cachedStatus.Status]
		if expSecs == -1 || time.Now().Unix() < cachedStatus.LastModified+expSecs {
			*useCache = true
		}
	}

	if *useCache {
		printStatus(cachedStatus.Status)
		os.Exit(0)
	}

	// Parse remote URL
	remoteURL, err := normalizeURL(runGit("config", "remote.origin.url"))
	if err != nil {
		die(fmt.Sprintf("Error while parsing URL: %s", err))
	}

	parts := strings.Split(remoteURL.Path, "/")
	if len(parts) < 3 {
		die(fmt.Sprintf("Could not parse: %q", remoteURL))
	}

	user := parts[1]
	repo := parts[2]

	// Setup client
	var httpClient *http.Client

	token := retrieveAPIToken(remoteURL)
	if token != "" {
		t := &oauth.Transport{
			Token: &oauth.Token{AccessToken: token},
		}
		httpClient = t.Client()
	}

	// Handle GitHub:Enterprise domains
	if remoteURL.Host != "github.com" {
		t := http.DefaultTransport.(*http.Transport)
		t.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	client := github.NewClient(httpClient)

	if remoteURL.Host != "github.com" {
		u, err := url.Parse(fmt.Sprintf("https://%s/api/v3/", remoteURL.Host))
		if err != nil {
			die(err.Error())
		}
		client.BaseURL = u
	}

	statuses, _, err := client.Repositories.ListStatuses(user, repo, rev, nil)
	if err != nil {
		die(fmt.Sprintf("Error while fetching status: %s", err))
	}

	thisStatus := programCacheStatus{
		Status:       "",
		LastModified: time.Now().Unix(),
	}

	if len(statuses) > 0 {
		thisStatus.Status = *statuses[0].State
	}

	printStatus(thisStatus.Status)

	if cache.Revisions == nil {
		cache.Revisions = map[string]programCacheStatus{}
	}
	cache.Revisions[rev] = thisStatus

	storeCache(cache, cacheFile)
}

func printStatus(status string) {
	mark := markUnknown

	switch status {
	case "failure":
		mark = markFailure
	case "pending":
		mark = markPending
	case "success":
		mark = markSuccess
	}

	if color, ok := statusColors[mark]; ok {
		ct.ChangeColor(color, false, ct.None, false)
		fmt.Print(mark)
		ct.ResetColor()
	} else {
		fmt.Print(mark)
	}
}
