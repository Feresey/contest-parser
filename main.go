package main

import (
	"context"
	"fmt"
	"io"
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

func main() {
	cli := &http.Client{
		Transport: http.DefaultTransport,
		Timeout:   5 * time.Second,
	}
	uri := "http://opentrains.snarknews.info/~ejudge/team.cgi?SID=7aeb0fad26706f08"

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		sig := <-c
		log.Warn("signal caught", zap.Stringer("signal", sig))
		cancel()
	}()

	se := &SummaryEmitter{}
	if err := Do(ctx, cli, uri, se); err != nil {
		log.Panic("process body", zap.Error(err))
	}
	log.Info("summary", zap.Reflect("emitted", se))

	pe := &ProblemsEmitter{}
	if err := Do(ctx, cli, se.SummaryHref, pe); err != nil {
		log.Panic("process body", zap.Error(err))
	}
	log.Info("problems", zap.Reflect("emitted", pe))
}

type Emitter interface {
	Emit(*goquery.Selection)
}

func Do(ctx context.Context, cli *http.Client, u string, emit Emitter) error {
	uri, err := url.Parse(u)
	if err != nil {
		panic(err)
	}

	log.Debug("url", zap.Reflect("url", u))
	req := &http.Request{
		Method: http.MethodGet,
		URL:    uri,
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

	return processBody(resp.Body, emit)
}

func processBody(body io.Reader, emitter Emitter) error {
	doc, err := goquery.NewDocumentFromReader(body)
	if err != nil {
		return err
	}
	emitter.Emit(doc.Selection)
	return nil
}

type SummaryEmitter struct {
	SummaryHref string
}

func (e *SummaryEmitter) Emit(doc *goquery.Selection) {
	doc.Find(".contest_actions_item a").EachWithBreak(func(i int, s *goquery.Selection) bool {
		if s.Text() == "Summary" {
			var exists bool
			e.SummaryHref, exists = s.Attr("href")

			if !exists {
				log.Error("Summary not exists")
			}
			return false
		}
		return true
	})
}

type Problem struct {
	ID    int
	Name  string
	RunID int
	OK    bool
}

type ProblemsEmitter struct {
	Problems []*Problem
}

func (p *ProblemsEmitter) Emit(doc *goquery.Selection) {
	sel := doc.Find(`table[class=b1] > tbody > tr`)

	res, _ := sel.Html()
	println(res)

	each := func(ss *[]string) func(i int, s *goquery.Selection) {
		return func(i int, s *goquery.Selection) {
			*ss = append(*ss, s.Text())
		}
	}

	var names []string
	first := sel.First()
	first.Children().Each(each(&names))

	sel.Next().Each(func(i int, s *goquery.Selection) {
		var cols []string
		s.Children().Each(each(&cols))
		problem, err := p.decodeProblem(names, cols)
		if err != nil {
			log.Error("decode problem", zap.Error(err))
			return
		}
		p.Problems = append(p.Problems, problem)
	})
}

func (p *ProblemsEmitter) decodeProblem(names, cols []string) (res *Problem, err error) {
	res = new(Problem)
	for idx, name := range names {
		switch name {
		case "Short name":
			res.ID, err = strconv.Atoi(cols[idx])
			if err != nil {
				err = fmt.Errorf("decode short name: %w", err)
			}
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
	if err != nil {
		log.Debug("decode problem", zap.Strings("names", names), zap.Strings("cols", cols))
	}
	return
}
