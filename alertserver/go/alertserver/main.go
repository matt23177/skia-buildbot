/*
	Provides roll-up statuses and alerting for Skia build/test/perf.
*/

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"
)

import (
	"github.com/fiorix/go-web/autogzip"
	"github.com/golang/glog"
	"github.com/influxdb/influxdb/client"
)

import (
	"skia.googlesource.com/buildbot.git/alertserver/go/alerting"
	"skia.googlesource.com/buildbot.git/alertserver/go/commit_cache"
	"skia.googlesource.com/buildbot.git/go/common"
	"skia.googlesource.com/buildbot.git/go/email"
	"skia.googlesource.com/buildbot.git/go/gitinfo"
	"skia.googlesource.com/buildbot.git/go/login"
	"skia.googlesource.com/buildbot.git/go/metadata"
	"skia.googlesource.com/buildbot.git/go/skiaversion"
	"skia.googlesource.com/buildbot.git/go/util"
)

const (
	COOKIESALT_METADATA_KEY          = "cookiesalt"
	CLIENT_ID_METADATA_KEY           = "client_id"
	CLIENT_SECRET_METADATA_KEY       = "client_secret"
	DEFAULT_COMMITS_TO_LOAD          = 35
	INFLUXDB_NAME_METADATA_KEY       = "influxdb_name"
	INFLUXDB_PASSWORD_METADATA_KEY   = "influxdb_password"
	GMAIL_CLIENT_ID_METADATA_KEY     = "gmail_clientid"
	GMAIL_CLIENT_SECRET_METADATA_KEY = "gmail_clientsecret"
	GMAIL_CACHED_TOKEN_METADATA_KEY  = "gmail_cached_token"
	GMAIL_TOKEN_CACHE_FILE           = "google_email_token.data"
)

var (
	alertManager *alerting.AlertManager    = nil
	gitInfo      *gitinfo.GitInfo          = nil
	commitCache  *commit_cache.CommitCache = nil

	alertsTemplate  *template.Template = nil
	commitsTemplate *template.Template = nil
)

// flags
var (
	graphiteServer        = flag.String("graphite_server", "localhost:2003", "Where is Graphite metrics ingestion server running.")
	host                  = flag.String("host", "localhost", "HTTP service host")
	port                  = flag.String("port", ":8001", "HTTP service port (e.g., ':8001')")
	useMetadata           = flag.Bool("use_metadata", true, "Load sensitive values from metadata not from flags.")
	influxDbHost          = flag.String("influxdb_host", "localhost:8086", "The InfluxDB hostname.")
	influxDbName          = flag.String("influxdb_name", "root", "The InfluxDB username.")
	influxDbPassword      = flag.String("influxdb_password", "root", "The InfluxDB password.")
	influxDbDatabase      = flag.String("influxdb_database", "", "The InfluxDB database.")
	emailClientIdFlag     = flag.String("email_clientid", "", "OAuth Client ID for sending email.")
	emailClientSecretFlag = flag.String("email_clientsecret", "", "OAuth Client Secret for sending email.")
	alertPollInterval     = flag.String("alert_poll_interval", "1s", "How often to check for new alerts.")
	alertsFile            = flag.String("alerts_file", "alerts.cfg", "Config file containing alert rules.")
	testing               = flag.Bool("testing", false, "Set to true for locally testing rules. No email will be sent.")
	workdir               = flag.String("workdir", ".", "Directory to use for scratch work.")
	resourcesDir          = flag.String("resources_dir", "", "The directory to find templates, JS, and CSS files. If blank the current directory will be used.")
)

// StringIsInteresting returns true iff the string contains non-whitespace characters.
func StringIsInteresting(s string) bool {
	for _, c := range s {
		if !unicode.IsSpace(c) {
			return true
		}
	}
	return false
}

func reloadTemplates() {
	// Change the current working directory to two directories up from this source file so that we
	// can read templates and serve static (res/) files.

	if *resourcesDir == "" {
		_, filename, _, _ := runtime.Caller(0)
		*resourcesDir = filepath.Join(filepath.Dir(filename), "../..")
	}
	alertsTemplate = template.Must(template.ParseFiles(
		filepath.Join(*resourcesDir, "templates/alerts.html"),
		filepath.Join(*resourcesDir, "templates/header.html"),
	))
	commitsTemplate = template.Must(template.ParseFiles(
		filepath.Join(*resourcesDir, "templates/commits.html"),
		filepath.Join(*resourcesDir, "templates/header.html"),
	))
}

func Init() {
	reloadTemplates()
}

func userHasEditRights(email string) bool {
	if strings.HasSuffix(email, "@google.com") {
		return true
	}
	return false
}

func getIntParam(name string, r *http.Request) (*int, error) {
	raw, ok := r.URL.Query()[name]
	if !ok {
		return nil, nil
	}
	v64, err := strconv.ParseInt(raw[0], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("Invalid value for parameter %q: %s -- %v", name, raw, err)
	}
	v32 := int(v64)
	return &v32, nil
}

func alertJsonHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	type displayComment struct {
		Time    int32  `json:"time"`
		User    string `json:"user"`
		Message string `json:"message"`
	}
	type displayAlert struct {
		Id           string            `json:"id"`
		Name         string            `json:"name"`
		Query        string            `json:"query"`
		Condition    string            `json:"condition"`
		Message      string            `json:"message"`
		Snoozed      bool              `json:"snoozed"`
		Triggered    int32             `json:"triggered"`
		SnoozedUntil int32             `json:"snoozedUntil"`
		Comments     []*displayComment `json:"comments"`
	}
	alerts := struct {
		Alerts []displayAlert `json:"alerts"`
	}{
		Alerts: []displayAlert{},
	}
	for _, a := range alertManager.Alerts() {
		comments := []*displayComment{}
		if a.Comments != nil {
			for _, c := range a.Comments {
				comments = append(comments, &displayComment{
					Time:    int32(c.Time.Unix()),
					User:    c.User,
					Message: c.Message,
				})
			}
		}
		alerts.Alerts = append(alerts.Alerts, displayAlert{
			Id:           a.Rule.Id,
			Name:         a.Rule.Name,
			Query:        a.Rule.Query,
			Condition:    a.Rule.Condition,
			Message:      a.Rule.Message,
			Snoozed:      a.Snoozed(),
			Triggered:    int32(a.Triggered().Unix()),
			SnoozedUntil: int32(a.SnoozedUntil().Unix()),
			Comments:     comments,
		})
	}
	bytes, err := json.Marshal(&alerts)
	if err != nil {
		glog.Error(err)
	}
	w.Write(bytes)
}

func alertHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		email := login.LoggedInAs(r)
		if !userHasEditRights(email) {
			util.ReportError(w, r, fmt.Errorf("User does not have edit rights."), "You must be logged in to an account with edit rights to do that.")
			return
		}
		// URLs take the form /alerts/<alertId>/<action>
		split := strings.Split(r.URL.String(), "/")
		if len(split) != 4 {
			util.ReportError(w, r, fmt.Errorf("Invalid URL %s", r.URL), "Requested URL is not valid.")
			return
		}
		alertId := split[2]
		if !alertManager.Contains(alertId) {
			util.ReportError(w, r, fmt.Errorf("Invalid Alert ID %s", alertId), "The requested resource does not exist.")
			return
		}
		action := split[3]
		if action == "dismiss" {
			glog.Infof("%s %s", action, alertId)
			alertManager.Dismiss(alertId, email)
			return
		} else if action == "snooze" {
			d := json.NewDecoder(r.Body)
			body := struct {
				Until int
			}{}
			err := d.Decode(&body)
			if err != nil || body.Until == 0 {
				util.ReportError(w, r, err, fmt.Sprintf("Unable to decode request body: %s", r.Body))
				return
			}
			defer r.Body.Close()
			until := time.Unix(int64(body.Until), 0)
			glog.Infof("%s %s until %v", action, alertId, until.String())
			alertManager.Snooze(alertId, until, email)
			return
		} else if action == "unsnooze" {
			glog.Infof("%s %s", action, alertId)
			alertManager.Unsnooze(alertId, email)
			return
		} else if action == "addcomment" {
			bytes, err := ioutil.ReadAll(r.Body)
			if err != nil {
				util.ReportError(w, r, err, fmt.Sprintf("Unable to read request body: %s", r.Body))
				return
			}
			defer r.Body.Close()
			comment := string(bytes)
			if !StringIsInteresting(comment) {
				util.ReportError(w, r, fmt.Errorf("Invalid comment text."), comment)
				return
			}
			glog.Infof("%s %s", action, alertId, comment)
			alertManager.AddComment(alertId, email, comment)
		} else {
			util.ReportError(w, r, fmt.Errorf("Invalid action %s", action), "The requested action is invalid.")
			return
		}
	}

	w.Header().Set("Content-Type", "text/html")

	// Don't use cached templates in testing mode.
	if *testing {
		reloadTemplates()
	}
	if err := alertsTemplate.Execute(w, struct{}{}); err != nil {
		glog.Errorln("Failed to expand template:", err)
	}
}

func makeResourceHandler() func(http.ResponseWriter, *http.Request) {
	fileServer := http.FileServer(http.Dir(*resourcesDir))
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Cache-Control", string(300))
		fileServer.ServeHTTP(w, r)
	}
}

func commitsJsonHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// Case 1: Requesting specific commit range by index.
	startIdx, err := getIntParam("start", r)
	if err != nil {
		util.ReportError(w, r, err, fmt.Sprintf("Invalid parameter: %v", err))
		return
	}
	if startIdx != nil {
		endIdx := commitCache.NumCommits()
		end, err := getIntParam("end", r)
		if err != nil {
			util.ReportError(w, r, err, fmt.Sprintf("Invalid parameter: %v", err))
			return
		}
		if end != nil {
			endIdx = *end
		}
		if err := commitCache.RangeAsJson(w, *startIdx, endIdx); err != nil {
			util.ReportError(w, r, err, fmt.Sprintf("Failed to load commit range from cache: %v", err))
			return
		}
		return
	}
	// Case 2: Requesting N (or the default number) commits.
	commitsToLoad := DEFAULT_COMMITS_TO_LOAD
	n, err := getIntParam("n", r)
	if err != nil {
		util.ReportError(w, r, err, fmt.Sprintf("Invalid parameter: %v", err))
		return
	}
	if n != nil {
		commitsToLoad = *n
	}
	if err := commitCache.LastNAsJson(w, commitsToLoad); err != nil {
		util.ReportError(w, r, err, fmt.Sprintf("Failed to load commits from cache: %v", err))
		return
	}
}

func commitsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")

	// Don't use cached templates in testing mode.
	if *testing {
		reloadTemplates()
	}

	if err := commitsTemplate.Execute(w, struct{}{}); err != nil {
		glog.Errorln("Failed to expand template:", err)
	}
}

func runServer(serverURL string) {
	http.HandleFunc("/res/", autogzip.HandleFunc(makeResourceHandler()))
	http.HandleFunc("/", alertHandler)
	http.HandleFunc("/commits", commitsHandler)
	http.HandleFunc("/json/alerts", alertJsonHandler)
	http.HandleFunc("/json/commits", commitsJsonHandler)
	http.HandleFunc("/json/version", skiaversion.JsonHandler)
	http.HandleFunc("/oauth2callback/", login.OAuth2CallbackHandler)
	http.HandleFunc("/logout/", login.LogoutHandler)
	http.HandleFunc("/loginstatus/", login.StatusHandler)
	glog.Infof("Ready to serve on %s", serverURL)
	glog.Fatal(http.ListenAndServe(*port, nil))
}

func main() {
	common.InitWithMetrics("alertserver", graphiteServer)
	v := skiaversion.GetVersion()
	glog.Infof("Version %s, built at %s", v.Commit, v.Date)

	Init()
	parsedPollInterval, err := time.ParseDuration(*alertPollInterval)
	if err != nil {
		glog.Fatalf("Failed to parse -alertPollInterval: %s", *alertPollInterval)
	}
	if *testing {
		*useMetadata = false
	}
	if *useMetadata {
		*influxDbName = metadata.MustGet(INFLUXDB_NAME_METADATA_KEY)
		*influxDbPassword = metadata.MustGet(INFLUXDB_PASSWORD_METADATA_KEY)
	}
	dbClient, err := client.New(&client.ClientConfig{
		Host:       *influxDbHost,
		Username:   *influxDbName,
		Password:   *influxDbPassword,
		Database:   *influxDbDatabase,
		HttpClient: nil,
		IsSecure:   false,
		IsUDP:      false,
	})
	if err != nil {
		glog.Fatalf("Failed to initialize InfluxDB client: %s", err)
	}
	serverURL := "https://" + *host
	if *testing {
		serverURL = "http://" + *host + *port
	}

	usr, err := user.Current()
	if err != nil {
		glog.Fatal(err)
	}
	tokenFile, err := filepath.Abs(usr.HomeDir + "/" + GMAIL_TOKEN_CACHE_FILE)
	if err != nil {
		glog.Fatal(err)
	}
	// By default use a set of credentials setup for localhost access.
	var cookieSalt = "notverysecret"
	var clientID = "31977622648-1873k0c1e5edaka4adpv1ppvhr5id3qm.apps.googleusercontent.com"
	var clientSecret = "cw0IosPu4yjaG2KWmppj2guj"
	var redirectURL = serverURL + "/oauth2callback/"
	var emailClientId = *emailClientIdFlag
	var emailClientSecret = *emailClientSecretFlag
	if *useMetadata {
		cookieSalt = metadata.MustGet(COOKIESALT_METADATA_KEY)
		clientID = metadata.MustGet(CLIENT_ID_METADATA_KEY)
		clientSecret = metadata.MustGet(CLIENT_SECRET_METADATA_KEY)
		emailClientId = metadata.MustGet(GMAIL_CLIENT_ID_METADATA_KEY)
		emailClientSecret = metadata.MustGet(GMAIL_CLIENT_SECRET_METADATA_KEY)
		cachedGMailToken := metadata.MustGet(GMAIL_CACHED_TOKEN_METADATA_KEY)
		err = ioutil.WriteFile(tokenFile, []byte(cachedGMailToken), os.ModePerm)
		if err != nil {
			glog.Fatalf("Failed to cache token: %s", err)
		}
	}
	login.Init(clientID, clientSecret, redirectURL, cookieSalt)

	var emailAuth *email.GMail
	if !*testing {
		if !*useMetadata && (emailClientId == "" || emailClientSecret == "") {
			glog.Fatal("If -use_metadata=false, you must provide -email_clientid and -email_clientsecret")
		}
		emailAuth, err = email.NewGMail(emailClientId, emailClientSecret, tokenFile)
		if err != nil {
			glog.Fatalf("Failed to create email auth: %v", err)
		}
	}
	alertManager, err = alerting.NewAlertManager(dbClient, *alertsFile, parsedPollInterval, emailAuth, *testing)
	if err != nil {
		glog.Fatalf("Failed to create AlertManager: %v", err)
	}
	glog.Info("Created AlertManager")

	gitInfo, err = gitinfo.CloneOrUpdate("https://skia.googlesource.com/skia.git", path.Join(*workdir, "skia"), true)
	if err != nil {
		glog.Fatalf("Failed to check out Skia: %v", err)
	}
	glog.Info("CloneOrUpdate complete")
	commitCache, err = commit_cache.New(gitInfo, path.Join(*workdir, "commit_cache.gob"))
	if err != nil {
		glog.Fatalf("Failed to create commit cache: %v", err)
	}
	glog.Info("commit_cache complete")
	runServer(serverURL)
}
