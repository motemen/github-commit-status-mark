package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"crypto/tls"

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

var rxDiagParts = map[string]*regexp.Regexp{}

func init() {
	for _, key := range []string{"hostandport", "path"} {
		rxDiagParts[key] = regexp.MustCompile(`^Diag: ` + key + `=(.+)`)
	}
}

func normalizeURL(urlString string) (*url.URL, error) {
	reScheme := regexp.MustCompile(`^[\w+]+://`)

	urlString = reScheme.ReplaceAllLiteralString(urlString, "https://")
	if strings.HasPrefix(urlString, "https://") == false {
		urlString = "https://" + strings.Replace(urlString, ":", "/", 1)
	}

	return url.Parse(strings.TrimSuffix(urlString, ".git"))
}

type programCacheStatus struct {
	Status       string
	LastModified int64
}

type programCache struct {
	Revisions map[string]programCacheStatus
}

func main() {
	useCache := flag.Bool("cached", false, "Output cached status")
	updateCache := flag.Bool("update", false, "Force fetch status")

	flag.Parse()

	// Obtain specified revision (or HEAD)
	rev := "HEAD"
	if len(flag.Args()) >= 1 {
		rev = flag.Arg(0)
	}
	rev = runGit("rev-parse", rev)

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

	cachedStatus := cache.Revisions[rev]

	expSecs := cacheExpirationSeconds[cachedStatus.Status]
	if expSecs == -1 || time.Now().Unix() < cachedStatus.LastModified+expSecs {
		*useCache = true
	}

	if *updateCache {
		*useCache = false
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

	token := runGit("config", "--get-urlmatch", "github.token", remoteURL.String())

	// Setup client
	var httpClient *http.Client

	if token != "" {
		t := &oauth.Transport{
			Token: &oauth.Token{AccessToken: token},
		}
		httpClient = t.Client()
	}

	if remoteURL.Host != "github.com" {
		// XXX
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

	cacheFile.Truncate(0)
	cacheFile.Seek(0, os.SEEK_SET)
	err = json.NewEncoder(cacheFile).Encode(&cache)
	if err != nil {
		die(err.Error())
	}
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
