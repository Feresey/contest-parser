package main

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/SebastiaanKlippert/go-wkhtmltopdf"
	"go.uber.org/zap"
)

type Emitter interface {
	Emit(context.Context, *goquery.Selection) error
}

func eachCol(ss *[]string) func(i int, s *goquery.Selection) {
	return func(i int, s *goquery.Selection) {
		*ss = append(*ss, s.Text())
	}
}

type HrefEmitter struct {
	originalHref *url.URL

	SummaryHref     *url.URL
	StatementsHref  *url.URL
	SubmissionsHref *url.URL
	StandingsHref   *url.URL
}

func (h *HrefEmitter) parseHref(text string, sel *goquery.Selection) (*url.URL, error) {
	s := sel.Find(fmt.Sprintf(`a:contains(%q)[href]`, text))
	summary, found := s.Attr("href")
	if !found {
		return nil, fmt.Errorf("%q href not found", text)
	}
	return h.originalHref.Parse(summary)
}

func (h *HrefEmitter) Emit(_ context.Context, doc *goquery.Selection) (err error) {
	actions := doc.Find(`[class=contest_actions_item]`)

	h.SummaryHref, err = h.parseHref("Summary", actions)
	if err != nil {
		return err
	}

	h.StandingsHref, err = h.parseHref("Standings", actions)
	if err != nil {
		return err
	}

	h.StandingsHref, err = h.parseHref("Standings", actions)
	if err != nil {
		return err
	}

	submissions, err := h.parseHref("Submissions", actions)
	if err != nil {
		return err
	}
	// add parameter
	q, err := url.ParseQuery(submissions.RawQuery)
	if err != nil {
		return err
	}
	q.Set("all_runs", "1")
	submissions.RawQuery = q.Encode()
	h.SubmissionsHref = submissions

	return nil
}

type Problem struct {
	ID    string
	Name  string
	RunID int
	OK    bool
}

type ProblemsEmitter struct {
	Problems     []*Problem
	SummaryTable string
}

func (pe *ProblemsEmitter) Emit(_ context.Context, doc *goquery.Selection) error {
	sel := doc.Find(`table[class=b1] > tbody > tr`)

	raw, err := sel.Html()
	if err != nil {
		return err
	}
	pe.SummaryTable = raw

	var names []string
	first := sel.First()
	first.Children().Each(eachCol(&names))

	var errRet error
	sel.Next().EachWithBreak(func(i int, s *goquery.Selection) bool {
		var cols []string
		s.Children().Each(eachCol(&cols))
		problem, err := pe.decodeProblem(names, cols)
		if err != nil {
			errRet = err
			log.Error("decode problem", zap.Error(err), zap.Strings("names", names), zap.Strings("cols", cols))
			return false
		}
		pe.Problems = append(pe.Problems, problem)
		return true
	})

	return errRet
}

func (pe *ProblemsEmitter) decodeProblem(names, cols []string) (res *Problem, err error) {
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

type StandingsEmitter struct {
	originalHref  *url.URL
	StandingsPage string
}

func (s *StandingsEmitter) Emit(_ context.Context, doc *goquery.Selection) error {
	link := doc.Find("link[href]")
	href, found := link.Attr("href")
	if found {
		u, err := s.originalHref.Parse(href)
		if err != nil {
			return fmt.Errorf("change href address: %w", err)
		}
		link.SetAttr("href", u.String())
	}
	raw, err := doc.Html()
	s.StandingsPage = raw
	return err
}

func (s *StandingsEmitter) GeneratePdf(w io.Writer) error {
	gen, err := wkhtmltopdf.NewPDFGenerator()
	if err != nil {
		return err
	}
	gen.AddPage(wkhtmltopdf.NewPageReader(strings.NewReader(s.StandingsPage)))
	if err := gen.Create(); err != nil {
		return err
	}
	_, err = gen.Buffer().WriteTo(w)
	return err
}
