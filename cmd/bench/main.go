// Command bench measures xpro-oss throughput (files -> target DB) in-process.
//
// It builds the same source -> parser -> target pipeline as main.go from
// config.yaml and runs it for every workers x batchSize combination, timing
// each run. Use -gen to build a synthetic corpus first.
//
//	# 1. make a corpus: N copies of a template with a unique value in one tag
//	go run ./cmd/bench -gen ./corpus/small -template sample.xml -count 20000 -tag OBJECT_ID
//
//	# 2. sweep (truncates the target before every run, so use a scratch table)
//	go run ./cmd/bench -dir ./corpus/small -workers 1,8,16,32 -batches 50,500 \
//	    -repeat 3 -truncate "TRUNCATE TABLE bench_records"
//
// config.yaml supplies the target and mapping; source.path is ignored in
// favor of -dir. Results are printed as a Markdown table (median of -repeat).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bluefunda/xpro-oss/btp"
	"github.com/bluefunda/xpro-oss/config"
	xdb "github.com/bluefunda/xpro-oss/db"
	"github.com/bluefunda/xpro-oss/parser"
	"github.com/bluefunda/xpro-oss/process"
)

func main() {
	var (
		cfgPath  = flag.String("config", "config.yaml", "xpro-oss config file (target + mapping)")
		dir      = flag.String("dir", "", "source directory of XML files to process")
		workers  = flag.String("workers", "1,4,8,16,32", "comma-separated worker counts")
		batches  = flag.String("batches", "50,500", "comma-separated batch sizes")
		repeat   = flag.Int("repeat", 3, "runs per combination (median is reported)")
		truncate = flag.String("truncate", "", "SQL executed before every run to empty the target table (required for sweeps)")
		limit    = flag.Int("limit", 0, "cap files per run (0 = all)")
		verbose  = flag.Bool("v", false, "print pipeline warnings/errors (e.g. why a batch failed) to stderr")

		gen      = flag.String("gen", "", "generate a corpus into this directory and exit")
		template = flag.String("template", "", "with -gen: template XML file")
		count    = flag.Int("count", 1000, "with -gen: number of files")
		tag      = flag.String("tag", "OBJECT_ID", "with -gen: element whose first value is made unique per file")
	)
	flag.Parse()

	if *gen != "" {
		if err := generate(*gen, *template, *count, *tag); err != nil {
			fatal(err)
		}
		return
	}
	if *dir == "" {
		fatal(fmt.Errorf("-dir is required (or use -gen)"))
	}
	if *truncate == "" {
		fatal(fmt.Errorf("-truncate is required so each run starts from an empty table"))
	}

	// The pipeline logs per run; keep output clean unless -v asks for warnings
	// (batch-level failures such as a refused connection are only visible there).
	logOut := io.Discard
	if *verbose {
		logOut = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}
	mapping, err := parser.Compile(cfg.Mapping)
	if err != nil {
		fatal(err)
	}
	ctx := context.Background()
	var resolve xdb.DestinationResolver
	if b := btp.Detect(); b != nil {
		resolve = b.Resolve
	}
	target, err := xdb.Open(ctx, cfg.Target, resolve)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = target.Close() }()

	files, bytes, err := corpusSize(*dir, *limit)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("# xpro-oss benchmark\n\n")
	fmt.Printf("- date: %s\n- target: %s (%s:%d)\n- corpus: %s — %d files, %.1f MB (avg %.1f KB)\n- cpus: %d, repeat: %d (median)\n\n",
		time.Now().UTC().Format(time.RFC3339), cfg.Target.DB, cfg.Target.Host, cfg.Target.Port,
		*dir, files, float64(bytes)/1e6, float64(bytes)/float64(files)/1e3, runtime.NumCPU(), *repeat)
	fmt.Println("| workers | batch | files/s | MB/s | secs | peak heap MB | failed |")
	fmt.Println("|---:|---:|---:|---:|---:|---:|---:|")
	errCounts := map[string]int{} // normalized error text -> occurrences

	for _, b := range ints(*batches) {
		for _, w := range ints(*workers) {
			var secs, heap []float64
			failed := 0
			for i := 0; i < *repeat; i++ {
				if err := target.Exec(ctx, *truncate); err != nil {
					fatal(fmt.Errorf("truncate: %w", err))
				}
				res, peak, err := runOnce(ctx, logger, *dir, mapping, target, w, b, *limit)
				if err != nil {
					fatal(err)
				}
				secs = append(secs, res.Elapsed.Seconds())
				heap = append(heap, peak)
				failed += res.Failed + (res.Files - res.Inserted - res.Failed)
				for _, e := range res.Errors {
					errCounts[normalizeErr(e)]++
				}
			}
			s, h := median(secs), median(heap)
			fmt.Printf("| %d | %d | %.0f | %.1f | %.2f | %.0f | %d |\n",
				w, b, float64(files)/s, float64(bytes)/1e6/s, s, h, failed)
		}
	}
	printErrors(errCounts)
}

// normalizeErr strips file names and numbers so identical failures group.
var numRe = regexp.MustCompile(`\d+`)

func normalizeErr(e string) string {
	if i := strings.Index(e, ": "); i >= 0 && strings.HasSuffix(e[:i], ".xml") {
		e = e[i+2:]
	}
	e = numRe.ReplaceAllString(e, "N")
	if len(e) > 200 {
		e = e[:200] + "…"
	}
	return e
}

func printErrors(m map[string]int) {
	if len(m) == 0 {
		return
	}
	type kv struct {
		k string
		v int
	}
	var list []kv
	for k, v := range m {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v })
	fmt.Printf("\n**Errors** (normalized, top %d):\n\n", min(5, len(list)))
	for i, e := range list {
		if i == 5 {
			break
		}
		fmt.Printf("- %d × `%s`\n", e.v, e.k)
	}
}

func runOnce(ctx context.Context, logger *slog.Logger, dir string, m []parser.Mapping, t *xdb.Inserter, workers, batch, limit int) (process.Result, float64, error) {
	p := &process.Pipeline{Source: os.DirFS(dir), Mapping: m, Target: t, BatchSize: batch, Workers: workers, Limit: limit, Logger: logger}

	// Sample heap in-use while the run is going; report the peak.
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	runtime.GC()
	res, err := p.Run(ctx)
	close(stop)
	<-done
	return res, float64(peak.Load()) / 1e6, err
}

func corpusSize(dir string, limit int) (files int, bytes int64, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if limit > 0 && files >= limit {
			break
		}
		info, err := e.Info()
		if err != nil {
			return 0, 0, err
		}
		files++
		bytes += info.Size()
	}
	if files == 0 {
		return 0, 0, fmt.Errorf("no files in %s", dir)
	}
	return files, bytes, nil
}

func generate(dir, template string, n int, tag string) error {
	if template == "" {
		return fmt.Errorf("-template is required with -gen")
	}
	src, err := os.ReadFile(template)
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`<` + regexp.QuoteMeta(tag) + `>[^<]*</` + regexp.QuoteMeta(tag) + `>`)
	if !re.Match(src) {
		return fmt.Errorf("template has no <%s> element", tag)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	loc := re.FindIndex(src) // first occurrence only
	for i := 0; i < n; i++ {
		val := fmt.Sprintf("<%s>%012d</%s>", tag, 700000000000+i, tag)
		body := append(append(append([]byte{}, src[:loc[0]]...), val...), src[loc[1]:]...)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%06d.xml", i)), body, 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("wrote %d files to %s\n", n, dir)
	return nil
}

func ints(csv string) []int {
	var out []int
	for _, s := range strings.Split(csv, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bench:", err)
	os.Exit(1)
}
