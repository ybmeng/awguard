package automations

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// registrationCalendar is the executable botnet calendar registration-ensure
// creates and files automation events under.
const registrationCalendar = "Automations"

// ensureTimeout bounds each botnet REST call during registration-ensure.
const ensureTimeout = 5 * time.Second

// ensureRegistered makes the botnet calendar able to fire every scheduled
// automation: the "Automations" calendar exists (executable), and each
// discovered automation with a schedule template has SOME event naming it.
//
// Ensure-if-absent only: an event that already carries automation == name —
// whatever calendar it sits on, whenever the user or a bot moved it to — is
// NEVER updated or touched. The calendar is authoritative; the manifest's
// schedule block is just the seed. Errors are returned for logging and
// retried on the next tick, never fatal.
func (s *Service) ensureRegistered(autos []Automation) error {
	if s.botnetAddr == "" {
		return nil // registration off; the service still discovers, fires and records
	}
	client := &http.Client{Timeout: ensureTimeout}
	base := "http://" + s.botnetAddr

	calID, err := s.ensureCalendar(client, base)
	if err != nil {
		return fmt.Errorf("registration-ensure: %w", err)
	}

	var registered []struct {
		Automation string `json:"automation"`
	}
	if err := getJSON(client, base+"/v1/events", &registered); err != nil {
		return fmt.Errorf("registration-ensure: list events: %w", err)
	}
	present := map[string]bool{}
	for _, ev := range registered {
		if ev.Automation != "" {
			present[ev.Automation] = true
		}
	}

	var errs []error
	for _, a := range autos {
		if a.Schedule == nil || present[a.Name] {
			continue
		}
		if err := s.createEvent(client, base, calID, a); err != nil {
			errs = append(errs, fmt.Errorf("registration-ensure: %s: %w", a.Name, err))
			continue
		}
		s.logger.Printf("automations: registered %s on the %q calendar (%s at %s %s)",
			a.Name, registrationCalendar, a.Schedule.RRULE, a.Schedule.At, a.Schedule.TZ)
	}
	return errors.Join(errs...)
}

// ensureCalendar returns the id of the "Automations" calendar, creating it
// (executable, color left for the botnet to assign) when absent.
func (s *Service) ensureCalendar(client *http.Client, base string) (string, error) {
	var calendars []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := getJSON(client, base+"/v1/calendars", &calendars); err != nil {
		return "", fmt.Errorf("list calendars: %w", err)
	}
	for _, c := range calendars {
		if strings.EqualFold(c.Name, registrationCalendar) {
			return c.ID, nil
		}
	}
	var created struct {
		ID string `json:"id"`
	}
	err := postJSON(client, base+"/v1/calendars",
		map[string]any{"name": registrationCalendar, "executable": true}, &created)
	if err != nil {
		return "", fmt.Errorf("create calendar: %w", err)
	}
	s.logger.Printf("automations: created the %q calendar (executable)", registrationCalendar)
	return created.ID, nil
}

// createEvent books one recurring event from a's schedule template: today's
// date at the template's wall-clock time in its tz (as a UTC instant), a
// window retry_for long, the template's rrule.
func (s *Service) createEvent(client *http.Client, base, calID string, a Automation) error {
	sc := a.Schedule
	loc, err := time.LoadLocation(sc.TZ)
	if err != nil {
		return fmt.Errorf("template tz %q: %w", sc.TZ, err)
	}
	at, err := time.Parse("15:04", sc.At)
	if err != nil {
		return fmt.Errorf("template at %q: %w", sc.At, err)
	}
	today := time.Now().In(loc)
	start := time.Date(today.Year(), today.Month(), today.Day(), at.Hour(), at.Minute(), 0, 0, loc).UTC()
	body := map[string]any{
		"calendarId": calID,
		"title":      a.Name,
		"startsAt":   start.Format(time.RFC3339),
		"endsAt":     start.Add(sc.RetryFor).Format(time.RFC3339),
		"rrule":      sc.RRULE,
		"tz":         sc.TZ,
		"automation": a.Name,
	}
	return postJSON(client, base+"/v1/events", body, nil)
}

func getJSON(client *http.Client, url string, out any) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, apiError(resp))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func postJSON(client *http.Client, url string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("POST %s: %s", url, apiError(resp))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// apiError extracts the botnet's {"error": ...} message, falling back to the
// status code.
func apiError(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return fmt.Sprintf("status %d: %s", resp.StatusCode, e.Error)
	}
	return fmt.Sprintf("status %d", resp.StatusCode)
}

// newVerifyBotnet is the in-memory botnet calendar stand-in Verify runs
// registration-ensure against: it lists what it holds and remembers what is
// POSTed, counting the writes.
func newVerifyBotnet(mu *sync.Mutex, calendars, events *[]map[string]any, writes *int) http.Handler {
	mux := http.NewServeMux()
	list := func(rows *[]map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if *rows == nil {
				*rows = []map[string]any{}
			}
			writeJSON(w, http.StatusOK, *rows)
		}
	}
	create := func(rows *[]map[string]any, idPrefix string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			defer mu.Unlock()
			body["id"] = idPrefix + fmt.Sprint(len(*rows))
			*rows = append(*rows, body)
			*writes++
			writeJSON(w, http.StatusCreated, body)
		}
	}
	mux.HandleFunc("GET /v1/calendars", list(calendars))
	mux.HandleFunc("POST /v1/calendars", create(calendars, "cal_"))
	mux.HandleFunc("GET /v1/events", list(events))
	mux.HandleFunc("POST /v1/events", create(events, "evt_"))
	return mux
}
