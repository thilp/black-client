package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/karrick/godirwalk"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	portHelp  = "TCP port blackd listens on."
	diffHelp  = "Don't write the files back, just output a diff for each file on stdout."
	checkHelp = "Don't write the files back, just return the status. " +
		"Return code 0 means nothing would change. Return code 1 means some files would be reformatted. " +
		"Return code 123 means there was an internal error."
)

var (
	port  = kingpin.Flag("port", portHelp).Required().Envar("BLACKD_PORT").Uint16()
	diff  = kingpin.Flag("diff", diffHelp).Bool()
	check = kingpin.Flag("check", checkHelp).Bool()
	files = kingpin.Arg("files", "Files to format").Strings()
)

type PathResult int

const (
	Unchanged PathResult = iota
	Reformatted
	WouldBeReformatted
	Error
)

func main() {
	log.SetFlags(0)
	kingpin.Parse()
	conf := BlackConfig{
		Url:   fmt.Sprintf("http://localhost:%s", strconv.FormatUint(uint64(*port), 10)),
		Check: *check,
		Diff:  *diff,
	}

	exitCode := 0
	unchangedCount := 0
	reformattedCount := 0
	errorCount := 0
	for _, path := range *files {
		err := godirwalk.Walk(path, &godirwalk.Options{
			FollowSymbolicLinks: true,
			Unsorted:            true,
			AllowNonDirectory:   true,
			Callback: func(path string, de *godirwalk.Dirent) error {
				log.Printf(">>> path=%v de=%v\n", path, *de)
				if (de.IsRegular() || de.IsSymlink()) && strings.HasSuffix(path, ".py") {
					switch processPath(conf, path) {
					case Unchanged:
						unchangedCount += 1
					case Reformatted:
						reformattedCount += 1
					case WouldBeReformatted:
						reformattedCount += 1
						if exitCode < 1 {
							exitCode = 1
						}
					case Error:
						errorCount += 1
						exitCode = 123
					}
				}
				return nil
			},
			ErrorCallback: func(path string, err error) godirwalk.ErrorAction {
				log.Printf("cannot format %s: %v", path, err)
				return godirwalk.SkipNode
			},
		})
		if err != nil {
			log.Fatalf("error traversing %s: %v", path, err)
		}
	}

	if unchangedCount == 0 && reformattedCount == 0 && errorCount == 0 {
		fmt.Println("No Python files are present to be formatted. Nothing to do 😴")
		return
	}

	report := strings.Builder{}
	if unchangedCount > 0 {
		reportCount(&report, conf.Check, unchangedCount, "would be left unchanged", "left unchanged")
	}
	if reformattedCount > 0 {
		reportCount(&report, conf.Check, reformattedCount, "would be reformatted", "reformatted")
	}
	if errorCount > 0 {
		reportCount(&report, conf.Check, errorCount, "would fail to reformat", "failed to reformat")
	}
	report.WriteRune('.')
	log.Println(report.String())
	os.Exit(exitCode)
}

func reportCount(buf *strings.Builder, check bool, count int, statusWithCheck, statusWithoutCheck string) {
	buf.Grow(10 + len(statusWithCheck))
	if buf.Len() > 0 {
		buf.WriteString(", ")
	}
	buf.WriteString(strconv.Itoa(count))
	buf.WriteString(" file")
	if count > 1 {
		buf.WriteString("s ")
	} else {
		buf.WriteRune(' ')
	}
	if check {
		buf.WriteString(statusWithCheck)
	} else {
		buf.WriteString(statusWithoutCheck)
	}
}

var client = &http.Client{}

func processPath(conf BlackConfig, path string) PathResult {
	resp, err := queryBlackd(conf, path)
	if err != nil {
		log.Print(err)
		return Error
	}
	defer resp.Body.Close()

	res, blackErr := newBlackResult(resp)
	if blackErr != nil {
		if blackErr.Syntax {
			log.Printf("%s: %s", path, blackErr.Msg)
			return Error
		}
		log.Printf("%s: blackd error: %s", path, blackErr.Msg)
		return Error
	}
	if !res.Changed {
		return Unchanged
	}
	if conf.Diff {
		_, err = io.Copy(os.Stdout, res.Text)
		if err != nil {
			log.Print(err)
			return Error
		}
	}
	if conf.Check {
		return WouldBeReformatted
	}
	if err = overwritePath(path, res.Text); err != nil {
		log.Print(err)
		return Error
	}
	return Reformatted
}

func queryBlackd(conf BlackConfig, path string) (*http.Response, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	defer file.Close()

	r := bufio.NewReader(file)
	req, err := http.NewRequest("POST", conf.Url, r)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}

	if conf.Diff {
		req.Header.Set("X-Diff", "1")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: couldn't reach blackd: %v", path, err)
	}
	return resp, nil
}

func newBlackResult(resp *http.Response) (*BlackResult, *BlackError) {
	switch resp.StatusCode {
	case 204:
		return &BlackResult{}, nil
	case 200:
		return &BlackResult{Changed: true, Text: resp.Body}, nil
	case 400:
		return nil, &BlackError{Syntax: true, Msg: newStringFromReader(resp.Body)}
	case 500:
		return nil, &BlackError{Msg: newStringFromReader(resp.Body)}
	}
	log.Fatalf("unsupported HTTP status: %v", resp.StatusCode)
	return nil, nil // never reached
}

func overwritePath(path string, newContents io.Reader) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("%s: cannot format: %v", path, err)
	}
	defer file.Close()
	w := bufio.NewWriter(file)
	_, err = io.Copy(w, newContents)
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		return fmt.Errorf("%s: formatting failed: %v", path, err)
	}
	return nil
}

type BlackConfig struct {
	Url   string
	Diff  bool
	Check bool
}

type BlackResult struct {
	Changed bool
	Text    io.Reader
}

type BlackError struct {
	Syntax bool
	Msg    string
}

func newStringFromReader(r io.Reader) string {
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(r)
	if err != nil {
		panic(err)
	}
	return buf.String()
}
