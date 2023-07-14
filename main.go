package main

import (
	"bytes"
	"context"
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/go-recaptcha/recaptcha"
	"github.com/gorilla/handlers"
	"github.com/kelseyhightower/envconfig"
	badge "github.com/narqo/go-badge"
	"github.com/nlopes/slack"
	xproxy "github.com/ory/x/proxy"
	"github.com/paulbellamy/ratecounter"
)

var indexTemplate = template.Must(template.New("index.tmpl").ParseFiles("templates/index.tmpl"))
var redirectTemplate = template.Must(template.New("redirect.tmpl").ParseFiles("templates/redirect.tmpl"))

var (
	api     *slack.Client
	captcha *recaptcha.Recaptcha
	counter *ratecounter.RateCounter

	ourTeam = new(team)

	m *expvar.Map
	hitsPerMinute,
	requests,
	inviteErrors,
	missingFirstName,
	missingLastName,
	missingEmail,
	missingCoC,
	successfulCaptcha,
	failedCaptcha,
	invalidCaptcha,
	successfulInvites,
	userCount,
	activeUserCount expvar.Int
)

var c Specification

type Traits struct {
	Email string
	Name  string
}

type Identity struct {
	Traits Traits
}

type SessionData struct {
	Identity Identity
}

// Specification is the config struct
type Specification struct {
	Port           string `envconfig:"PORT" required:"true"`
	CaptchaSitekey string `required:"true"`
	CaptchaSecret  string `required:"true"`
	SlackToken     string `required:"true"`
	CocUrl         string `required:"false" default:"http://coc.golangbridge.org/"`
	SessionData    json.RawMessage
	EnforceHTTPS   bool
	Debug          bool // toggles nlopes/slack client's debug flag
}

type contextKey string

const (
	oryUpstreamAPI     = "project.console.ory.sh"
	oryUpstreamConsole = "console.ory.sh"
)

var oryAPIPath = regexp.MustCompile(`\/\.ory\/*`)
var oryUIPath = regexp.MustCompile(`\/\.ui\/*`)

func init() {
	var showUsage = flag.Bool("h", false, "Show usage")
	flag.Parse()

	if *showUsage {
		err := envconfig.Usage("slackinviter", &c)
		if err != nil {
			log.Fatal(err.Error())
		}
		os.Exit(0)
	}

	err := envconfig.Process("slackinviter", &c)
	if err != nil {
		log.Fatal(err.Error())
	}
	counter = ratecounter.NewRateCounter(1 * time.Minute)
	m = expvar.NewMap("metrics")
	m.Set("hits_per_minute", &hitsPerMinute)
	m.Set("requests", &requests)
	m.Set("invite_errors", &inviteErrors)
	m.Set("missing_first_name", &missingFirstName)
	m.Set("missing_last_name", &missingLastName)
	m.Set("missing_email", &missingEmail)
	m.Set("missing_coc", &missingCoC)
	m.Set("failed_captcha", &failedCaptcha)
	m.Set("invalid_captcha", &invalidCaptcha)
	m.Set("successful_captcha", &successfulCaptcha)
	m.Set("successful_invites", &successfulInvites)
	m.Set("active_user_count", &activeUserCount)
	m.Set("user_count", &userCount)

	captcha = recaptcha.New(c.CaptchaSecret)
	api = slack.New(c.SlackToken, slack.OptionDebug(c.Debug))
}

func handleBadge(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	users := userCount.String()
	if activeUserCount.Value() > 0 {
		users = activeUserCount.String() + "/" + userCount.String()
	}

	var buf bytes.Buffer
	if err := badge.Render("slack", users, "#E01563", &buf); err != nil {
		log.Fatal(err)
	}
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	buf.WriteTo(w)
}

func main() {
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(4)

	go pollSlack(ctx, &wg)
	go server(ctx, &wg)
	go cleanup(cancel, c)

	wg.Wait()
}

func server(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	mux := http.NewServeMux()
	mux.HandleFunc("/invite/", handleInvite)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))
	/* 	mux.HandleFunc("/", enforceHTTPSFunc(sessionCheck((redirectPage)))) */
	mux.HandleFunc("/badge.svg", handleBadge)
	mux.Handle("/debug/vars", http.DefaultServeMux)
	mux.HandleFunc("/sessiondata", handleSessionData)
	mux.HandleFunc("/invitation", homepage)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {

		if !oryAPIPath.MatchString(r.URL.Path) && !oryUIPath.MatchString(r.URL.Path) {
			homepage(w, r)
			return
		}

		var upstream string
		var prefix string

		if oryUIPath.MatchString(r.URL.Path) {
			upstream = oryUpstreamConsole
			prefix = "/.ui"
		} else {
			upstream = oryUpstreamAPI
			prefix = "/.ory"
		}

		xproxy.New(
			func(_ context.Context, r *http.Request) (*xproxy.HostConfig, error) {
				return &xproxy.HostConfig{
					CookieDomain:   r.Host,
					UpstreamHost:   upstream,
					UpstreamScheme: "https",
					TargetHost:     upstream,
					PathPrefix:     prefix,
					CorsEnabled:    false,
				}, nil
			},
			xproxy.WithReqMiddleware(func(r *http.Request, c *xproxy.HostConfig, body []byte) ([]byte, error) {
				log.Println("Proxy Request to Upstream...")
				log.Printf("Request: %+v\n", r)

				if r.URL.Host == upstream {
					r.Host = upstream
				}

				// r.Header.Set("Ory-No-Custom-Domain-Redirect", "true")
				// r.Header.Set("Ory-Base-URL-Rewrite", "http://localhost:3000")
				// r.Header.Set("Ory-Base-URL-Rewrite-Token", apiKey)

				return body, nil
			}),
			xproxy.WithRespMiddleware(func(resp *http.Response, config *xproxy.HostConfig, body []byte) ([]byte, error) {
				log.Println("Proxy Response from Upstream...")

				log.Printf("Response: %+v\n", resp)
				l, err := resp.Location()
				if err == nil {
					if l.Host == oryUpstreamConsole {
						upstream = oryUpstreamConsole
						prefix = ".ui"
					}
					if l.Host == oryUpstreamAPI {
						upstream = oryUpstreamAPI
						prefix = ".ory"
					}

					newLocation := l

					if newLocation.Host == upstream {
						newLocation.Scheme = "http"
						newLocation.Host = "localhost:3000"
						if !strings.HasPrefix(newLocation.Path, prefix) {
							newLocation.Path = filepath.Join(prefix, newLocation.Path)
						}
					}

					resp.Header.Set("Location", newLocation.String())
				}

				body = []byte(strings.ReplaceAll(string(body), "/_next/", "/.ui/_next/"))
				body = []byte(strings.ReplaceAll(string(body), "/__ENV.js", "/.ui/__ENV.js"))
				body = []byte(strings.ReplaceAll(string(body), "/manifest.json", "/.ui/manifest.json"))
				body = []byte(strings.ReplaceAll(string(body), "/favicon.ico", "/.ui/favicon.ico"))
				body = []byte(strings.ReplaceAll(string(body), "/assets", "/.ui/assets"))
				body = []byte(strings.ReplaceAll(string(body), "https://project.console.ory.sh", "http://localhost:3000/.ory"))
				body = []byte(strings.ReplaceAll(string(body), "http://project.console.ory.sh", "http://localhost:3000/.ory"))
				body = []byte(strings.ReplaceAll(string(body), "\"rpId\":\"console.ory.sh\"", "\"rpId\":\"localhost:3000\""))

				// 	newLocation := l
				// 	newLocation.Scheme = "http"
				// 	newLocation.Host = "localhost:3000"
				//
				// 	newLocation.Path = filepath.Join(".ory", newLocation.Path)
				//
				// 	resp.Header.Set("Location", newLocation.String())
				// }

				return body, nil
			}),
		).ServeHTTP(w, r)
	})

	err := http.ListenAndServe(":"+c.Port, handlers.CombinedLoggingHandler(os.Stdout, mux))
	if err != nil {
		log.Fatal(err.Error())
	}
}

func cleanup(cancel context.CancelFunc, c chan os.Signal) {
	<-c
	cancel()
	os.Exit(1)
}

func sessionCheck(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), "GET", fmt.Sprintf("http://%s/.ory/sessions/whoami", r.Host), nil)
		if err != nil {
			http.Error(w, "Something unexpected went wrong\n"+err.Error(), http.StatusInternalServerError)
			return
		}

		for _, cookie := range r.Cookies() {
			req.AddCookie(cookie)
		}

		req.Header.Set("Accept", "application/json")

		c := &http.Client{}
		resp, err := c.Do(req)
		if err != nil {
			http.Error(w, "LOL Something unexpected went wrong", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			http.Redirect(w, r, "/.ory/self-service/login/browser?refresh=true", http.StatusSeeOther)
			return
		}

		h.ServeHTTP(w, r)
	}
}

func enforceHTTPSFunc(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if xfp := r.Header.Get("X-Forwarded-Proto"); c.EnforceHTTPS && xfp == "http" {
			u := *r.URL
			u.Scheme = "https"
			if u.Host == "" {
				u.Host = r.Host
			}
			http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
			return
		}
		h(w, r)
	}
}

// Updates the globals from the slack API
// returns the length of time to sleep before the function
// should be called again
func updateFromSlack(ctx context.Context) time.Duration {
	var (
		err            error
		p              slack.UserPagination
		uCount, aCount int64 // users and active users
	)

	for p = api.GetUsersPaginated(
		slack.GetUsersOptionPresence(true),
		slack.GetUsersOptionLimit(500),
	); !p.Done(err); p, err = p.Next(ctx) {
		if err != nil {
			if rle, ok := err.(*slack.RateLimitedError); ok {
				fmt.Printf("Being Rate Limited by Slack: %s\n", rle)
				time.Sleep(rle.RetryAfter)
				continue
			}
		}
		for _, u := range p.Users {
			if u.ID != "USLACKBOT" && !u.IsBot && !u.Deleted {
				uCount++
				if u.Presence == "active" {
					aCount++
				}
			}
		}
		fmt.Println("User Count:", uCount)
		fmt.Println("Active Count:", aCount)
	}
	userCount.Set(uCount)
	activeUserCount.Set(aCount)
	if err != nil && !p.Done(err) {
		log.Println("error polling slack for users:", err)
		return time.Minute
	}

	st, err := api.GetTeamInfo()
	if err != nil {
		log.Println("error polling slack for team info:", err)
		return time.Minute
	}
	ourTeam.Update(st)
	return time.Hour
}

// pollSlack over and over again
func pollSlack(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		time.Sleep(updateFromSlack(ctx))
	}
}

const sessionDataKey contextKey = "sessionData"

func handleSessionData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse the JSON request body
	var responseData map[string]interface{}
	err := json.NewDecoder(r.Body).Decode(&responseData)
	if err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}
	log.Println("responseData", responseData)

	// Extract the desired traits data
	traitsData, ok := responseData["identity"].(map[string]interface{})["traits"].(map[string]interface{})
	if !ok {
		http.Error(w, "Invalid traits data", http.StatusBadRequest)
		return
	}

	// Build the session data struct
	var sessionData = SessionData{
		Identity: Identity{
			Traits: Traits{
				Email: traitsData["email"].(string),
				Name:  traitsData["name"].(string),
			},
		},
	}
	log.Println("session data in handleSessionData:", sessionData) // debug <-- here is some issue
	// Store the session data in the request context

	// Perform the redirect with sessionData as a query parameter
	redirectURL := "/invitation?email=" + url.QueryEscape(sessionData.Identity.Traits.Email) + "&name=" + url.QueryEscape(sessionData.Identity.Traits.Name)
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// homepage renders the homepage
func homepage(w http.ResponseWriter, r *http.Request) {
	log.Println("session data in homepage:", r.URL.Query().Get("email"))
	counter.Incr(1)
	hitsPerMinute.Set(counter.Rate())
	requests.Add(1)
	// Check if session data is available in the query parameters
	email := r.URL.Query().Get("email")
	name := r.URL.Query().Get("name")

	// Build the session data struct
	sessionData := &SessionData{
		Identity: Identity{
			Traits: Traits{
				Email: email,
				Name:  name,
			},
		},
	}

	// Render the index template with sessionData
	renderTemplate(w, sessionData)
}

// renders the redirect page
func redirectPage(w http.ResponseWriter, r *http.Request) {
	counter.Incr(1)
	hitsPerMinute.Set(counter.Rate())
	requests.Add(1)
	log.Println("session data:", r.Context().Value(sessionDataKey))

	// Render the redirect template without sessionData
	redirectTemplate.Execute(w, nil)
}

// renderTemplate executes the index template with the provided sessionData
func renderTemplate(w http.ResponseWriter, sessionData *SessionData) {
	var buf bytes.Buffer
	err := indexTemplate.Execute(
		&buf,
		struct {
			SiteKey,
			UserCount,
			ActiveCount string
			Team        *team
			CocUrl      string
			SessionData *SessionData
		}{
			c.CaptchaSitekey,
			userCount.String(),
			activeUserCount.String(),
			ourTeam,
			c.CocUrl,
			sessionData,
		},
	)
	if err != nil {
		log.Println("error rendering template:", err)
		http.Error(w, "error rendering template :-(", http.StatusInternalServerError)
		return
	}

	// Set the header and write the buffer to the http.ResponseWriter
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// ShowPost renders a single post
func handleInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	successfulCaptcha.Add(1)
	fname := r.FormValue("fname")
	lname := r.FormValue("lname")
	email := r.FormValue("email")
	coc := r.FormValue("coc")
	if email == "" {
		missingEmail.Add(1)
		http.Error(w, "Missing email", http.StatusPreconditionFailed)
		return
	}
	if fname == "" {
		missingFirstName.Add(1)
		http.Error(w, "Missing first name", http.StatusPreconditionFailed)
		return
	}
	if lname == "" {
		missingLastName.Add(1)
		http.Error(w, "Missing last name", http.StatusPreconditionFailed)
		return
	}
	if coc != "1" {
		missingCoC.Add(1)
		http.Error(w, "You need to accept the code of conduct", http.StatusPreconditionFailed)
		return
	}
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		failedCaptcha.Add(1)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	captchaResponse := r.FormValue("g-recaptcha-response")
	valid, err := captcha.Verify(captchaResponse, remoteIP)
	if err != nil {
		failedCaptcha.Add(1)
		http.Error(w, "Error validating recaptcha.. Did you click it?", http.StatusPreconditionFailed)
		return
	}
	if !valid {
		invalidCaptcha.Add(1)
		http.Error(w, "Invalid recaptcha", http.StatusInternalServerError)
		return

	}
	// all is well, let's try to invite someone!
	err = api.InviteToTeam(ourTeam.Domain(), fname, lname, email)
	if err != nil {
		log.Println("InviteToTeam error:", err)
		inviteErrors.Add(1)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	successfulInvites.Add(1)
}
