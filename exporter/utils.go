// SPDX-FileCopyrightText: 2023 Rivos Inc.
//
// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"io"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/exp/slog"
)

type SlurmPrimitiveMetric interface {
	NodeMetric | JobMetric | DiagMetric | LicenseMetric | AccountLimitMetric
}

// interface for getting data from slurm
// used for dep injection/ease of testing & for add slurmrestd support later
type SlurmByteScraper interface {
	FetchRawBytes() ([]byte, error)
	Duration() time.Duration
}

type SlurmMetricFetcher[M SlurmPrimitiveMetric] interface {
	FetchMetrics() ([]M, error)
	ScrapeDuration() time.Duration
	ScrapeError() prometheus.Counter
}

type AtomicThrottledCache[C SlurmPrimitiveMetric] struct {
	sync.Mutex
	t     time.Time
	limit float64
	cache []C
	// duration of last cache miss
	duration time.Duration
}

// atomic fetch of either the cache or the collector
// reset & hydrate as necessary
func (atc *AtomicThrottledCache[C]) FetchOrThrottle(fetchFunc func() ([]C, error)) ([]C, error) {
	atc.Lock()
	defer atc.Unlock()
	if len(atc.cache) > 0 && time.Since(atc.t).Seconds() < atc.limit {
		return atc.cache, nil
	}
	t := time.Now()
	slurmData, err := fetchFunc()
	if err != nil {
		return nil, err
	}
	atc.duration = time.Since(t)
	atc.cache = slurmData
	atc.t = time.Now()
	return slurmData, nil
}

func NewAtomicThrottledCache[C SlurmPrimitiveMetric](limit float64) *AtomicThrottledCache[C] {
	return &AtomicThrottledCache[C]{
		t:     time.Now(),
		limit: limit,
	}
}

func track(cmd []string) (string, time.Time) {
	return strings.Join(cmd, " "), time.Now()
}

func duration(msg string, start time.Time) {
	slog.Debug(fmt.Sprintf("cmd %s took %s secs", msg, time.Since(start)))
}

// implements SlurmByteScraper by fetch data from cli
type CliScraper struct {
	args     []string
	timeout  time.Duration
	duration time.Duration
}

func (cf *CliScraper) Duration() time.Duration {
	return cf.duration
}

func (cf *CliScraper) FetchRawBytes() ([]byte, error) {
	defer func(t time.Time) { cf.duration = time.Since(t) }(time.Now())
	if len(cf.args) == 0 {
		return nil, errors.New("need at least 1 args")
	}
	defer duration(track(cf.args))
	cmd := exec.Command(cf.args[0], cf.args[1:]...)
	var outb, errb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	timer := time.AfterFunc(cf.timeout, func() {
		if err := cmd.Process.Kill(); err != nil {
			slog.Error(fmt.Sprintf("failed to cancel cmd: %v", cf.args))
		}
	})
	defer timer.Stop()
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	if errb.Len() > 0 {
		return nil, fmt.Errorf("cmd failed with %s", errb.String())
	}
	
	if (cf.args[0] != "sinfo" || !strings.Contains(cf.args[len(cf.args)-1],`{"s": "%T", "mem": %m, "n": "%n", "l": "%O", "p": "%R", "fmem": "%e", "cstate": "%C", "w": %w}`)) {
		return outb.Bytes(), nil
	}

	sinfoMainJsonify := exec.Command("jq", []string{"-s", "-c", "."}...)
	sinfoMainJsonify.Stdin = &outb

	sinfoMainRead, sinfoMainWrite := io.Pipe()
	sinfoMainJsonify.Stdout = sinfoMainWrite

	if err := sinfoMainJsonify.Start(); err != nil {
		return nil, err
	}

	go func() {
		sinfoMainJsonify.Wait()
		sinfoMainWrite.Close()
	}()

	sinfoAMemRaw := exec.Command("sinfo", []string{"--all","-e","-h","-O","NodeAddr,PartitionName,AllocMem"}...)
	sinfoAMemJsonifyRead, sinfoAMemJsonifyWrite := io.Pipe()
	sinfoAMemRaw.Stdout = sinfoAMemJsonifyWrite

	if err := sinfoAMemRaw.Start(); err != nil {
		return nil, err
	}

	go func() {
		sinfoAMemRaw.Wait() // Ensure sinfoAMemRaw is properly waited upon
		sinfoAMemJsonifyWrite.Close()
	}()

	sinfoAMemJsonify := exec.Command("sh", "-c", "tr -s ' ' | jq -nR -c '[inputs | split(\" \") | {n: .[0], p: .[1], amem: .[2]|tonumber }]'")
	sinfoAMemJsonify.Stdin = sinfoAMemJsonifyRead
	sinfoAMemRead, sinfoAMemWrite := io.Pipe()
	sinfoAMemJsonify.Stdout = sinfoAMemWrite

	if err := sinfoAMemJsonify.Start(); err != nil {
		return nil, err
	}

	go func() {
		sinfoAMemJsonify.Wait()
		sinfoAMemWrite.Close()
	}()

	combinedReader := io.MultiReader(sinfoMainRead, sinfoAMemRead)

	/*
	processedOutput, _ = io.ReadAll(combinedReader)
	slog.Error(fmt.Sprintf("%s","combinedReader json"))
    slog.Error(fmt.Sprintf("%s",string(processedOutput)))
	*/

	combined := exec.Command("jq",[]string{"-c","--slurp",".[0][] as $file1 | .[1][] as $file2 | select($file1.n == $file2.n and $file1.p == $file2.p) | ($file1 + $file2)"}...)

	combined.Stdin = combinedReader

	var combinedOut,combinedErr bytes.Buffer
	combined.Stdout = &combinedOut
	combined.Stderr = &combinedErr

	if err := combined.Start(); err != nil {
		return nil, err
	}


	if err := combined.Wait(); err != nil {
		slog.Error(fmt.Sprintf("failed to wait for combined jq command: %v", err))
		slog.Error(fmt.Sprintf("failed to wait for combined jq command: %v", combinedErr.String()))
	}

	// slog.Info(fmt.Sprintf("new sinfo: %s",combinedOut.String()))

	return combinedOut.Bytes(), nil
}

func NewCliScraper(args ...string) *CliScraper {
	var limit float64 = 10
	var err error
	if tm, ok := os.LookupEnv("CLI_TIMEOUT"); ok {
		if limit, err = strconv.ParseFloat(tm, 64); err != nil {
			slog.Error("`CLI_TIMEOUT` env var parse error")
		}
	}
	return &CliScraper{
		args:    args,
		timeout: time.Duration(limit) * time.Second,
	}
}

// convert slurm mem string to float64 bytes
func MemToFloat(mem string) (float64, error) {
	if num, err := strconv.ParseFloat(mem, 64); err == nil {
		return num, nil
	}
	memUnits := map[string]float64{
		"M": 1e+6,
		"G": 1e+9,
		"T": 1e+12,
	}
	re := regexp.MustCompile(`^(?P<num>([0-9]*[.])?[0-9]+)(?P<memunit>G|M|T)$`)
	matches := re.FindStringSubmatch(mem)
	if len(matches) < 2 {
		return -1, fmt.Errorf("mem string %s doesn't match regex %s nor is a float", mem, re)
	}
	// err here should be impossible due to regex
	num, err := strconv.ParseFloat(matches[re.SubexpIndex("num")], 64)
	memunit := memUnits[matches[re.SubexpIndex("memunit")]]
	return num * memunit, err
}
