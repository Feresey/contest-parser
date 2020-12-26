package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/PuerkitoBio/goquery"
)

var (
	lc     = zap.NewDevelopmentConfig()
	log, _ = lc.Build()
)

func main() {
	var p Parser

	flag.StringVar(&p.Username, "username", "msknord13", "")
	flag.StringVar(&p.Password, "password", "", "required")
	flag.IntVar(&p.ContestID, "contest-id", 10521, "context id (10521, 10523, ...)")
	flag.StringVar(&p.BaseURL, "url", "http://opentrains.snarknews.info/~ejudge/team.cgi", "path to contest site")
	flag.StringVar(&p.Output, "o", "contests", "path to output dir")
	flag.BoolVar(&p.Force, "force", false, "overwrite output dir")
	flag.Parse()

	p.cli = &http.Client{
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

	err := p.Run(ctx)
	if err != nil {
		log.Error("run parser", zap.Error(err))
	}
	log.Info("run parser succeeded")
}

type Emitters struct {
	HrefEmitter
	ProblemsEmitter
	SubmissionsEmitter
	StandingsEmitter
}

type Parser struct {
	Username, Password string
	ContestID          int
	BaseURL            string
	Output             string
	Force              bool

	cli *http.Client

	Emitters
}

func (p *Parser) InitEmitters(u *url.URL) {
	p.SubmissionsEmitter.cli = p.cli
	p.HrefEmitter.originalHref = u
	p.StandingsEmitter.originalHref = u
}

func (p *Parser) loginContest(ctx context.Context) (*url.URL, error) {
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return nil, err
	}

	q := make(url.Values)
	q.Set("login", p.Username)
	q.Set("password", p.Password)
	q.Set("role", "0")
	q.Set("locale_id", "0")
	q.Set("submit", "Log in")
	q.Set("contest_id", strconv.Itoa(p.ContestID))

	u.RawQuery = q.Encode()

	log.Debug("url", zap.Stringer("url", u))
	req := &http.Request{
		Method: http.MethodGet,
		URL:    u,
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req = req.WithContext(cctx)
	resp, err := p.cli.Do(req)
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

func (p *Parser) Run(ctx context.Context) error {
	if err := p.GetData(ctx); err != nil {
		log.Error("get data", zap.Error(err))
		return err
	}

	return p.WriteData(p.Output)
}

func processBody(ctx context.Context, body io.Reader, emitter Emitter) error {
	doc, err := goquery.NewDocumentFromReader(body)
	if err != nil {
		return err
	}
	return emitter.Emit(ctx, doc.Selection)
}

func (p *Parser) Do(ctx context.Context, u *url.URL, emit Emitter) error {
	req := &http.Request{
		Method: http.MethodGet,
		URL:    u,
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req = req.WithContext(cctx)
	resp, err := p.cli.Do(req)
	if err != nil {
		log.Error("do request", zap.Error(err), zap.Stringer("url", u))
		return err
	}
	defer resp.Body.Close()

	return processBody(cctx, resp.Body, emit)
}

func (p *Parser) GetData(ctx context.Context) error {
	uri, err := p.loginContest(ctx)
	if err != nil {
		log.Error("login failed", zap.Error(err))
		return err
	}
	log.Debug("context url", zap.Stringer("url", uri))

	p.InitEmitters(uri)

	if err := p.Do(ctx, uri, &p.HrefEmitter); err != nil {
		log.Error("parse hrefs", zap.Error(err))
		return err
	}

	for _, runData := range []struct {
		Emitter
		URL *url.URL
	}{
		{&p.ProblemsEmitter, p.SummaryHref},
		{&p.StandingsEmitter, p.StandingsHref},
		{&p.SubmissionsEmitter, p.SubmissionsHref},
	} {
		log := log.With(
			zap.Stringer("url", runData.URL),
			zap.String("emitter", fmt.Sprintf("%T", runData.Emitter)),
		)
		if err := p.Do(ctx, runData.URL, runData.Emitter); err != nil {
			log.Error("emit", zap.Error(err))
			return err
		}
		log.Info("emit")
	}

	return nil
}

func fileName(lang string) string {
	switch _ = lang; {
	case strings.Contains(lang, "g++"):
		return "main.cpp"
	case strings.Contains(lang, "gcc"):
		return "main.c"
	case strings.Contains(lang, "python"):
		return "main.py"
	default:
		panic(lang)
	}
}

func (p *Parser) WriteData(out string) error {
	stat, err := os.Stat(out)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}
	if stat != nil {
		if stat.IsDir() && !p.Force {
			return errors.New("output directory exists")
		}
		if !stat.IsDir() {
			return errors.New("output path is file")
		}
	}
	if err := os.MkdirAll(out, os.ModePerm); err != nil {
		return err
	}

	standingsOut := filepath.Join(out, "standings.pdf")
	standingsFile, err := os.Create(standingsOut)
	if err != nil {
		return fmt.Errorf("standings: %w", err)
	}
	defer standingsFile.Close() //?

	if err := p.StandingsEmitter.GeneratePdf(standingsFile); err != nil {
		return fmt.Errorf("generate standings: %w", err)
	}

	problemsMap := make(map[string]*Problem)
	for _, problem := range p.Problems {
		problemsMap[problem.ID] = problem
	}

	for _, submission := range p.Submissions {
		problem, ok := problemsMap[submission.ProblemID]
		if !ok {
			return fmt.Errorf("problem %q not found", submission.ProblemID)
		}
		problemDir := filepath.Join(out, problem.ID)
		if err := os.MkdirAll(problemDir, os.ModePerm); err != nil {
			return fmt.Errorf("create problem dir: %q: %w", problemDir, err)
		}
		path := filepath.Join(problemDir, fileName(submission.Language))
		err := ioutil.WriteFile(path, submission.Source, 0644)
		if err != nil {
			return fmt.Errorf("write file: %q: %w", path, err)
		}
	}

	return nil
}
