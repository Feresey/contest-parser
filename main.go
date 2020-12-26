package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
	"go.uber.org/zap"
)

var (
	lc     = zap.NewDevelopmentConfig()
	log, _ = lc.Build()
)

func loginContest(
	ctx context.Context,
	cli *http.Client,
	uri string,
	username, password string,
	contestID int,
) (*url.URL, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	q := make(url.Values)
	q.Set("login", username)
	q.Set("password", password)
	q.Set("role", "0")
	q.Set("locale_id", "0")
	q.Set("submit", "Log in")
	q.Set("contest_id", strconv.Itoa(contestID))

	u.RawQuery = q.Encode()

	log.Debug("url", zap.Stringer("url", u))
	req := &http.Request{
		Method: http.MethodGet,
		URL:    u,
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req = req.WithContext(cctx)
	resp, err := cli.Do(req)
	if err != nil {
		log.Error("do request", zap.Error(err))
		return nil, err
	}
	defer resp.Body.Close()
	log.Debug("code", zap.Int("code", resp.StatusCode))

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}

	href, found := doc.Find(`.user_actions .contest_actions_item > a`).Attr("href")
	if !found {
		raw, _ := doc.Html()
		print(raw)
		return nil, fmt.Errorf("href not found")
	}

	return url.Parse(href)
}

func main() {
	var (
		username, password string
		contestID          int
		baseURL            string
		output             string
	)

	flag.StringVar(&username, "username", "msknord13", "")
	flag.StringVar(&password, "password", "", "")
	flag.IntVar(&contestID, "contest-id", 10521, "context id (10521, 10523, ...)")
	flag.StringVar(&baseURL, "url", "http://opentrains.snarknews.info/~ejudge/team.cgi", "path to contest site")
	flag.StringVar(&output, "o", "out.json", "path to output file with contest data")
	flag.Parse()

	cli := &http.Client{
		Transport: http.DefaultTransport,
		Timeout:   5 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		sig := <-c
		log.Warn("signal caught", zap.Stringer("signal", sig))
		cancel()
	}()

	uri, err := loginContest(ctx, cli, baseURL, username, password, contestID)
	if err != nil {
		log.Panic("login failed", zap.Error(err))
	}
	log.Debug("context url", zap.Stringer("url", uri))

	he := &HrefEmitter{}
	if err := Do(ctx, cli, uri, he); err != nil {
		log.Panic("parse hrefs", zap.Error(err))
	}

	pe := &ProblemsEmitter{}
	if err := Do(ctx, cli, he.SummaryHref, pe); err != nil {
		log.Panic("parse problems", zap.Error(err))
	}

	se := &SubmissionsEmitter{
		cli: cli,
	}
	if err := Do(ctx, cli, he.SubmissionsHref, se); err != nil {
		log.Panic("parse submissions", zap.Error(err))
	}

	out, err := os.Create(output)
	if err != nil {
		panic(err)
	}
	defer out.Close()
	enc := json.NewEncoder(out)
	err = enc.Encode(map[string]interface{}{
		"Problems":    pe.Problems,
		"Submissions": se.Submissions,
	})
	if err != nil {
		log.Panic("encode", zap.Error(err))
	}
}

type Emitter interface {
	Emit(context.Context, *goquery.Selection) error
}

func Do(ctx context.Context, cli *http.Client, u *url.URL, emit Emitter) error {
	log.Debug("url", zap.Reflect("url", u))
	req := &http.Request{
		Method: http.MethodGet,
		URL:    u,
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req = req.WithContext(cctx)
	resp, err := cli.Do(req)
	if err != nil {
		log.Error("do request", zap.Error(err))
		return err
	}
	defer resp.Body.Close()

	return processBody(cctx, resp.Body, emit)
}

func processBody(ctx context.Context, body io.Reader, emitter Emitter) error {
	doc, err := goquery.NewDocumentFromReader(body)
	if err != nil {
		return err
	}
	return emitter.Emit(ctx, doc.Selection)
}

type HrefEmitter struct {
	SummaryHref     *url.URL
	SubmissionsHref *url.URL
}

func (e *HrefEmitter) Emit(_ context.Context, doc *goquery.Selection) (err error) {
	actions := doc.Find(`[class=contest_actions_item]`)

	summary, found := actions.Find(`a:contains("Summary")[href]`).Attr("href")
	if !found {
		return fmt.Errorf("Summary href not found")
	}
	e.SummaryHref, err = url.Parse(summary)
	if err != nil {
		return err
	}

	submissions, found := actions.Find(`a:contains("Submissions")[href]`).Attr("href")
	if !found {
		return fmt.Errorf("Submissions href not found")
	}

	// add parameter
	u, err := url.Parse(submissions)
	if err != nil {
		return err
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return err
	}

	q.Set("all_runs", "1")
	u.RawQuery = q.Encode()
	e.SubmissionsHref = u

	return nil
}

func eachCol(ss *[]string) func(i int, s *goquery.Selection) {
	return func(i int, s *goquery.Selection) {
		*ss = append(*ss, s.Text())
	}
}

type Problem struct {
	ID    string
	Name  string
	RunID int
	OK    bool
}

type ProblemsEmitter struct {
	Problems []*Problem
}

func (p *ProblemsEmitter) Emit(_ context.Context, doc *goquery.Selection) error {
	sel := doc.Find(`table[class=b1] > tbody > tr`)

	var names []string
	first := sel.First()
	first.Children().Each(eachCol(&names))

	var errRet error
	sel.Next().EachWithBreak(func(i int, s *goquery.Selection) bool {
		var cols []string
		s.Children().Each(eachCol(&cols))
		problem, err := p.decodeProblem(names, cols)
		if err != nil {
			errRet = err
			log.Error("decode problem", zap.Error(err), zap.Strings("names", names), zap.Strings("cols", cols))
			return false
		}
		p.Problems = append(p.Problems, problem)
		return true
	})

	return errRet
}

func (p *ProblemsEmitter) decodeProblem(names, cols []string) (res *Problem, err error) {
	res = new(Problem)
	for idx, name := range names {
		switch name {
		case "Short name":
			res.ID = cols[idx]
		case "Long name":
			res.Name = cols[idx]
		case "Status":
			res.OK = cols[idx] == "OK"
		case "Run ID":
			if !res.OK {
				continue
			}
			res.RunID, err = strconv.Atoi(cols[idx])
			if err != nil {
				err = fmt.Errorf("decode run id: %w", err)
			}
		}
	}
	return
}

type Submission struct {
	ProblemID  string
	Language   string
	sourceHref *url.URL
	Source     []byte
	OK         bool
}

type SubmissionsEmitter struct {
	cli         *http.Client
	Submissions []*Submission
}

func (se *SubmissionsEmitter) Emit(ctx context.Context, doc *goquery.Selection) error {
	sel := doc.Find(`table[class=b1] > tbody > tr`)

	var (
		names             []string
		uniqueSubmissions = make(map[string]struct{})
		errRet            error
	)

	first := sel.First()
	first.Children().Each(eachCol(&names))

	sel.Next().EachWithBreak(func(i int, s *goquery.Selection) bool {
		var cols []string
		s.Children().Each(eachCol(&cols))
		submission, err := se.decodeSubmission(names, cols)
		if err != nil {
			errRet = err
			log.Error("decode problem", zap.Error(err), zap.Strings("names", names), zap.Strings("cols", cols))
			return false
		}
		href, ok := s.Children().Find(`a:contains("View")[href]`).Attr("href")
		if !ok {
			errRet = fmt.Errorf("href to source not found")
			return false
		}
		submission.sourceHref, err = url.Parse(href)
		if err != nil {
			errRet = err
			return false
		}

		if _, ok := uniqueSubmissions[submission.ProblemID]; ok {
			return true
		}
		se.Submissions = append(se.Submissions, submission)
		uniqueSubmissions[submission.ProblemID] = struct{}{}
		return true
	})
	if errRet != nil {
		return errRet
	}

	return se.loadSource(ctx)
}

func (se *SubmissionsEmitter) decodeSubmission(names, cols []string) (res *Submission, err error) {
	res = new(Submission)
	for idx, name := range names {
		switch name {
		case "Problem":
			res.ProblemID = cols[idx]
		case "Language":
			res.Language = cols[idx]
		case "Result":
			res.OK = cols[idx] == "OK"

			// case "View source":
			// 	res.SourceHref, err = url.Parse(cols[idx])
		}
	}
	return
}

func (se *SubmissionsEmitter) loadSource(ctx context.Context) error {
	for _, submission := range se.Submissions {
		raw, err := se.fetchSource(ctx, submission.sourceHref)
		if err != nil {
			return fmt.Errorf("fetch url: %s: %v", submission.sourceHref.String(), err)
		}
		submission.Source = raw
	}
	return nil
}

func (se *SubmissionsEmitter) fetchSource(ctx context.Context, u *url.URL) ([]byte, error) {
	req := &http.Request{
		Method: http.MethodGet,
		URL:    u,
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req = req.WithContext(cctx)
	resp, err := se.cli.Do(req)
	if err != nil {
		log.Error("do request", zap.Error(err), zap.Stringer("url", u))
		return nil, err
	}
	defer resp.Body.Close()

	return ioutil.ReadAll(resp.Body)
}
