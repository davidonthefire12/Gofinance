package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

//go:embed templates/*.html
var tplFS embed.FS

var tpl = template.Must(template.ParseFS(tplFS, "templates/*.html"))

// ---------------------------------------------------------------- config

type config struct {
	port, pin, secret   string
	sbURL, sbKey, table string
	selfURL             string
	loc                 *time.Location
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// loadDotEnv reads KEY=VALUE lines from .env (if present) without
// overriding variables that are already set in the environment.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func loadConfig() config {
	loadDotEnv(".env")
	c := config{
		port:    env("PORT", "8080"),
		pin:     os.Getenv("APP_PIN"),
		secret:  os.Getenv("SESSION_SECRET"),
		sbURL:   strings.TrimRight(os.Getenv("SUPABASE_URL"), "/"),
		sbKey:   os.Getenv("SUPABASE_KEY"),
		table:   env("SUPABASE_TABLE", "transactions"),
		selfURL: strings.TrimRight(env("KEEPALIVE_URL", os.Getenv("RENDER_EXTERNAL_URL")), "/"),
	}
	if c.sbURL == "" || c.sbKey == "" || c.pin == "" {
		log.Fatal("SUPABASE_URL, SUPABASE_KEY and APP_PIN are required")
	}
	if c.secret == "" {
		b := make([]byte, 32)
		rand.Read(b)
		c.secret = hex.EncodeToString(b)
		log.Println("SESSION_SECRET not set: using a random one (sessions reset on restart)")
	}
	loc, err := time.LoadLocation(env("TZ_NAME", "Asia/Bangkok"))
	if err != nil {
		loc = time.UTC
	}
	c.loc = loc
	return c
}

// ---------------------------------------------------------------- supabase

type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	*f = flexID(strings.Trim(string(b), `"`))
	return nil
}

// Expected columns: id, date (date), type (text), amount (numeric), note (text)
type Tx struct {
	ID     flexID  `json:"id"`
	Date   string  `json:"date"`
	Type   string  `json:"paymentType"`
	Amount float64 `json:"amount"`
	Note   string  `json:"note"`
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func (a *app) sb(method string, q url.Values, body any, prefer string) ([]byte, error) {
	u := a.cfg.sbURL + "/rest/v1/" + a.cfg.table
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", a.cfg.sbKey)
	req.Header.Set("Authorization", "Bearer "+a.cfg.sbKey)
	req.Header.Set("Content-Type", "application/json")
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("supabase %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}

func (a *app) listYear(year int) ([]Tx, error) {
	var all []Tx
	for off := 0; ; off += 1000 {
		q := url.Values{}
		q.Set("select", "*")
		q.Add("date", fmt.Sprintf("gte.%04d-01-01", year))
		q.Add("date", fmt.Sprintf("lt.%04d-01-01", year+1))
		q.Set("order", "date.desc,createdAt.desc")
		q.Set("limit", "1000")
		q.Set("offset", strconv.Itoa(off))
		data, err := a.sb("GET", q, nil, "")
		if err != nil {
			return nil, err
		}
		var page []Tx
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 1000 {
			return all, nil
		}
	}
}

type txBody struct {
	Date   string  `json:"date"`
	Type   string  `json:"paymentType"`
	Amount float64 `json:"amount"`
	Note   string  `json:"note"`
}

func (a *app) create(b txBody) error {
	_, err := a.sb("POST", nil, b, "return=minimal")
	return err
}

func (a *app) update(id string, b txBody) error {
	q := url.Values{}
	q.Set("id", "eq."+id)
	_, err := a.sb("PATCH", q, b, "return=minimal")
	return err
}

func (a *app) remove(id string) error {
	q := url.Values{}
	q.Set("id", "eq."+id)
	_, err := a.sb("DELETE", q, nil, "return=minimal")
	return err
}

// ---------------------------------------------------------------- view model

type Row struct {
	ID         string
	DateDisp   string
	Type       string
	Note       string
	AmountDisp string
	IsIncome   bool
}

type EditForm struct {
	ID, Date, Type, Amount, Note string
}

type Opt struct {
	N    int
	Name string
	Sel  bool
}

type SumRow struct {
	Name, Income, Expense, Balance string
	Current                        bool
	BalNeg                         bool
}

type YM struct{ Year, Month int }

type View struct {
	Year, Month                 int
	MonthName                   string
	Prev, Next, Today           YM
	Months                      []Opt
	Years                       []Opt
	Rows                        []Row
	Edit                        *EditForm
	Types                       []string
	Income, Expense, Balance    string
	BalNeg                      bool
	YIncome, YExpense, YBalance string
	YBalNeg                     bool
	Summary                     []SumRow
	Error                       string
}

type Page struct {
	Authed bool
	Error  string
	V      *View
}

type viewOpts struct {
	EditID string
	New    bool
	Form   *EditForm
	Err    string
}

var defaultTypes = []string{"Salary", "Food", "Utilities", "Health", "Entertainment", "Transport", "Shopping", "Other"}

func money(f float64) string {
	neg := f < 0
	if neg {
		f = -f
	}
	s := fmt.Sprintf("%.2f", f)
	ip, fp := s[:len(s)-3], s[len(s)-2:]
	var out []byte
	for i, c := range []byte(ip) {
		if i > 0 && (len(ip)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	r := "฿" + string(out) + "." + fp
	if neg {
		r = "-" + r
	}
	return r
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func isIncome(t string) bool { return strings.EqualFold(strings.TrimSpace(t), "salary") }

func shiftMonth(y, m, d int) YM {
	m += d
	if m < 1 {
		m, y = 12, y-1
	} else if m > 12 {
		m, y = 1, y+1
	}
	return YM{y, m}
}

func (a *app) buildView(year, month int, o viewOpts) (*View, error) {
	txs, err := a.listYear(year)
	if err != nil {
		return nil, err
	}
	now := time.Now().In(a.cfg.loc)
	v := &View{Year: year, Month: month, MonthName: time.Month(month).String(), Error: o.Err}
	v.Prev, v.Next = shiftMonth(year, month, -1), shiftMonth(year, month, 1)
	v.Today = YM{now.Year(), int(now.Month())}

	for m := 1; m <= 12; m++ {
		v.Months = append(v.Months, Opt{m, time.Month(m).String(), m == month})
	}
	for y := now.Year() - 6; y <= now.Year()+2; y++ {
		v.Years = append(v.Years, Opt{y, strconv.Itoa(y), y == year})
	}

	var mi, me float64
	var yi, ye float64
	var sumI, sumE [12]float64
	seen := map[string]bool{}
	v.Types = append(v.Types, defaultTypes...)
	for _, t := range defaultTypes {
		seen[strings.ToLower(t)] = true
	}
	prefix := fmt.Sprintf("%04d-%02d", year, month)
	for _, t := range txs {
		if len(t.Date) < 10 {
			continue
		}
		mm, _ := strconv.Atoi(t.Date[5:7])
		if mm < 1 || mm > 12 {
			continue
		}
		amt := abs(t.Amount)
		inc := isIncome(t.Type)
		if inc {
			yi += amt
			sumI[mm-1] += amt
		} else {
			ye += amt
			sumE[mm-1] += amt
		}
		if t.Type != "" && !seen[strings.ToLower(t.Type)] {
			seen[strings.ToLower(t.Type)] = true
			v.Types = append(v.Types, t.Type)
		}
		if t.Date[:7] != prefix {
			continue
		}
		r := Row{
			ID:       string(t.ID),
			DateDisp: t.Date[8:10] + "/" + t.Date[5:7] + "/" + t.Date[0:4],
			Type:     t.Type,
			Note:     t.Note,
			IsIncome: inc,
		}
		if inc {
			mi += amt
			r.AmountDisp = money(amt)
		} else {
			me += amt
			r.AmountDisp = money(-amt)
		}
		v.Rows = append(v.Rows, r)
	}

	v.Income, v.Expense, v.Balance, v.BalNeg = money(mi), money(me), money(mi-me), mi-me < 0
	v.YIncome, v.YExpense, v.YBalance, v.YBalNeg = money(yi), money(ye), money(yi-ye), yi-ye < 0
	names := []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	for i := 0; i < 12; i++ {
		v.Summary = append(v.Summary, SumRow{names[i], money(sumI[i]), money(sumE[i]), money(sumI[i] - sumE[i]), i+1 == month, sumI[i]-sumE[i] < 0})
	}

	switch {
	case o.Form != nil:
		v.Edit = o.Form
	case o.EditID != "":
		for _, t := range txs {
			if string(t.ID) == o.EditID {
				v.Edit = &EditForm{ID: o.EditID, Date: safeDate(t.Date), Type: t.Type,
					Amount: strconv.FormatFloat(abs(t.Amount), 'f', -1, 64), Note: t.Note}
			}
		}
	case o.New:
		d := fmt.Sprintf("%04d-%02d-01", year, month)
		if v.Today.Year == year && v.Today.Month == month {
			d = now.Format("2006-01-02")
		}
		v.Edit = &EditForm{Date: d, Type: "Food"}
	}
	return v, nil
}

func safeDate(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// ---------------------------------------------------------------- app & auth

type app struct {
	cfg config
	lim *limiter
}

func (a *app) sign(exp int64) string {
	m := hmac.New(sha256.New, []byte(a.cfg.secret))
	fmt.Fprintf(m, "%d", exp)
	return fmt.Sprintf("%d.%s", exp, hex.EncodeToString(m.Sum(nil)))
}

func (a *app) authed(r *http.Request) bool {
	c, err := r.Cookie("session")
	if err != nil {
		return false
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(a.sign(exp)), []byte(c.Value))
}

func (a *app) setSession(w http.ResponseWriter, r *http.Request, on bool) {
	c := &http.Cookie{Name: "session", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.Header.Get("X-Forwarded-Proto") == "https"}
	if on {
		exp := time.Now().Add(30 * 24 * time.Hour)
		c.Value, c.Expires = a.sign(exp.Unix()), exp
	} else {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

type limiter struct {
	mu sync.Mutex
	m  map[string][]time.Time
}

func (l *limiter) prune(ip string) []time.Time {
	var keep []time.Time
	for _, t := range l.m[ip] {
		if time.Since(t) < 15*time.Minute {
			keep = append(keep, t)
		}
	}
	l.m[ip] = keep
	return keep
}
func (l *limiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(ip)) >= 5
}
func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[ip] = append(l.prune(ip), time.Now())
}

func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	return h
}

func render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Println("template:", err)
		http.Error(w, "render error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}

func (a *app) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			render(w, "login", Page{})
			return
		}
		h(w, r)
	}
}

func (a *app) ym(r *http.Request) (int, int) {
	now := time.Now().In(a.cfg.loc)
	y, _ := strconv.Atoi(r.FormValue("year"))
	m, _ := strconv.Atoi(r.FormValue("month"))
	if y < 2000 || y > 2100 {
		y = now.Year()
		if r.FormValue("year") == "" && r.FormValue("month") == "" {
			m = int(now.Month())
		}
	}
	if m < 1 || m > 12 {
		m = int(now.Month())
	}
	return y, m
}

func (a *app) showApp(w http.ResponseWriter, year, month int, o viewOpts) {
	v, err := a.buildView(year, month, o)
	if err != nil {
		log.Println(err)
		http.Error(w, "Could not load data from Supabase. Check SUPABASE_URL, SUPABASE_KEY and table name.", 502)
		return
	}
	render(w, "app", v)
}

// ---------------------------------------------------------------- handlers

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !a.authed(r) {
		render(w, "page", Page{})
		return
	}
	y, m := a.ym(r)
	v, err := a.buildView(y, m, viewOpts{})
	if err != nil {
		log.Println(err)
		http.Error(w, "Could not load data from Supabase.", 502)
		return
	}
	render(w, "page", Page{Authed: true, V: v})
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if a.lim.blocked(ip) {
		render(w, "login", Page{Error: "Too many attempts. Try again in 15 minutes."})
		return
	}
	pin := r.FormValue("pin")
	if subtle.ConstantTimeCompare([]byte(pin), []byte(a.cfg.pin)) != 1 {
		a.lim.fail(ip)
		time.Sleep(400 * time.Millisecond)
		render(w, "login", Page{Error: "Incorrect PIN."})
		return
	}
	a.setSession(w, r, true)
	y, m := a.ym(r)
	a.showApp(w, y, m, viewOpts{})
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	a.setSession(w, r, false)
	render(w, "login", Page{})
}

func (a *app) appView(w http.ResponseWriter, r *http.Request) {
	y, m := a.ym(r)
	a.showApp(w, y, m, viewOpts{EditID: r.FormValue("edit"), New: r.FormValue("new") == "1"})
}

func (a *app) save(w http.ResponseWriter, r *http.Request) {
	y, m := a.ym(r)
	f := &EditForm{
		ID:     r.FormValue("id"),
		Date:   r.FormValue("date"),
		Type:   strings.TrimSpace(r.FormValue("type")),
		Amount: strings.TrimSpace(r.FormValue("amount")),
		Note:   strings.TrimSpace(r.FormValue("note")),
	}
	d, derr := time.Parse("2006-01-02", f.Date)
	amt, aerr := strconv.ParseFloat(f.Amount, 64)
	switch {
	case derr != nil:
		a.showApp(w, y, m, viewOpts{Form: f, Err: "Enter a valid date."})
		return
	case aerr != nil || amt <= 0:
		a.showApp(w, y, m, viewOpts{Form: f, Err: "Enter an amount greater than 0."})
		return
	case f.Type == "":
		a.showApp(w, y, m, viewOpts{Form: f, Err: "Choose a type."})
		return
	}
	body := txBody{Date: f.Date, Type: f.Type, Amount: amt, Note: f.Note}
	var err error
	if f.ID == "" {
		err = a.create(body)
	} else {
		err = a.update(f.ID, body)
	}
	if err != nil {
		log.Println(err)
		a.showApp(w, y, m, viewOpts{Form: f, Err: "Could not save the row. Try again."})
		return
	}
	a.showApp(w, d.Year(), int(d.Month()), viewOpts{})
}

func (a *app) del(w http.ResponseWriter, r *http.Request) {
	y, m := a.ym(r)
	var msg string
	if id := r.FormValue("id"); id != "" {
		if err := a.remove(id); err != nil {
			log.Println(err)
			msg = "Could not delete the row. Try again."
		}
	}
	a.showApp(w, y, m, viewOpts{Err: msg})
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("deep") == "1" {
		q := url.Values{}
		q.Set("select", "id")
		q.Set("limit", "1")
		if _, err := a.sb("GET", q, nil, ""); err != nil {
			log.Println("health:", err)
			http.Error(w, "supabase unreachable", 503)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, "ok")
}

// keepAlive pings the public URL so Render sees inbound traffic and does not
// spin the service down after 15 idle minutes. The deep ping also runs a tiny
// query so the Supabase free project is not paused for inactivity.
func (a *app) keepAlive() {
	if a.cfg.selfURL == "" {
		log.Println("keep-alive disabled (no RENDER_EXTERNAL_URL / KEEPALIVE_URL)")
		return
	}
	target := a.cfg.selfURL + "/healthz?deep=1"
	log.Println("keep-alive pinging", target, "every 10 minutes")
	time.Sleep(30 * time.Second)
	for {
		resp, err := httpClient.Get(target)
		if err != nil {
			log.Println("keep-alive:", err)
		} else {
			resp.Body.Close()
		}
		time.Sleep(10 * time.Minute)
	}
}

func main() {
	a := &app{cfg: loadConfig(), lim: &limiter{m: map[string][]time.Time{}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /", a.index)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("GET /app", a.auth(a.appView))
	mux.HandleFunc("POST /rows/save", a.auth(a.save))
	mux.HandleFunc("POST /rows/delete", a.auth(a.del))

	go a.keepAlive()
	log.Println("listening on :" + a.cfg.port)
	log.Fatal(http.ListenAndServe(":"+a.cfg.port, mux))
}
