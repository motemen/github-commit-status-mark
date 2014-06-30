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
	statusUnknown = ""
	statusFailure = "failure"
	statusPending = "pending"
	statusSuccess = "success"
)

const forever = time.Duration(-1)

var statusConfiguration = map[string]struct {
	mark     string
	color    ct.Color
	cacheFor time.Duration
}{
	statusUnknown: {"?", ct.None, 30 * time.Second},
	statusFailure: {"✗", ct.Red, forever},
	statusPending: {"●", ct.Yellow, 10 * time.Second},
	statusSuccess: {"✓", ct.Green, forever},
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

func dieIf(err error) {
	if err != nil {
		die(err.Error())
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

func targetRevision(args []string) string {
	rev := "HEAD"
	if len(args) >= 1 {
		rev = args[0]
	}
	rev = runGit("rev-parse", rev)

	return rev
}

func restoreState() persistentState {
	var state persistentState

	cacheFilePath := filepath.Join(
		runGit("rev-parse", "--show-toplevel"),
		".github-commit-status",
		"cache",
	)

	cacheFile, err := os.Open(cacheFilePath)
	if err == nil {
		json.NewDecoder(cacheFile).Decode(&state)
	} else {
		if os.IsNotExist(err) {
			// ok
		} else {
			die(err.Error())
		}
	}

	return state
}

func saveState(state persistentState) {
	cacheFilePath := filepath.Join(
		runGit("rev-parse", "--show-toplevel"),
		".github-commit-status",
		"cache",
	)

	cacheFile, err := os.Create(cacheFilePath)
	dieIf(err)

	err = json.NewEncoder(cacheFile).Encode(&state)
	dieIf(err)
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

type revisionEntry struct {
	Status       string
	LastModified int64
}

type persistentState struct {
	Revisions map[string]revisionEntry
}

func main() {
	var (
		useCache    = flag.Bool("cached", false, "Output cached status")
		updateCache = flag.Bool("update", false, "Force fetch status")
	)
	flag.Parse()

	rev := targetRevision(flag.Args())

	state := restoreState()

	cachedRevisionEntry := state.Revisions[rev]

	conf, ok := statusConfiguration[cachedRevisionEntry.Status]
	if !ok {
		conf = statusConfiguration[statusUnknown]
	}

	if *updateCache {
		*useCache = false
	} else {
		exp := conf.cacheFor
		if exp == forever || time.Now().Before(time.Unix(cachedRevisionEntry.LastModified, 0).Add(exp)) {
			*useCache = true
		}
	}

	if *useCache {
		printStatus(cachedRevisionEntry.Status)
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
		dieIf(err)

		client.BaseURL = u
	}

	statuses, _, err := client.Repositories.ListStatuses(user, repo, rev, nil)
	if err != nil {
		die(fmt.Sprintf("Error while fetching status: %s", err))
	}

	thisStatus := revisionEntry{
		Status:       "",
		LastModified: time.Now().Unix(),
	}

	if len(statuses) > 0 {
		thisStatus.Status = *statuses[0].State
	}

	printStatus(thisStatus.Status)

	if state.Revisions == nil {
		state.Revisions = map[string]revisionEntry{}
	}
	state.Revisions[rev] = thisStatus

	saveState(state)
}

func printStatus(status string) {
	conf, ok := statusConfiguration[status]
	if !ok {
		conf = statusConfiguration[statusUnknown]
	}

	ct.ChangeColor(conf.color, false, ct.None, false)
	fmt.Print(conf.mark)
	ct.ResetColor()
}
