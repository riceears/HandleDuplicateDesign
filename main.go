package main

import (
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math/bits"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/corona10/goimagehash"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Pipeline:
// Images → Perceptual hash → LSH bucket → BK-tree search trong bucket → Hamming filter → Group → Keep largest → Delete → Log

const normalizeMaxSide = 256
const progressStep = 200

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true,
}

type imageRecord struct {
	Path   string
	Width  int
	Height int
	Pixels int64
	Hash   *goimagehash.ImageHash
	Err    error
}

func main() {
	root := flag.String("root", ".", "Thư mục gốc chứa ảnh (quét đệ quy)")
	maxDist := flag.Int("distance", 10, "Ngưỡng khoảng cách Hamming giữa hai perceptual hash (càng nhỏ càng khắt khe)")
	lshBits := flag.Int("lsh-bits", 2, "Số bit mỗi band LSH (1,2,4,8,16 — phải chia hết 64). Band càng nhỏ càng nhiều bucket, an toàn với -distance lớn")
	batchSize := flag.Int("batch-size", 5000, "Số ảnh xử lý mỗi batch trong Phase 1")
	dryRun := flag.Bool("dry-run", false, "Chỉ ghi log, không xóa file")
	logPath := flag.String("log", "dedupe_design.log", "Đường dẫn file log")
	normalizedArgs, positionalRoot := normalizeCLIArgs(os.Args[1:])
	if err := flag.CommandLine.Parse(normalizedArgs); err != nil {
		log.Fatal(err)
	}

	// Hỗ trợ truyền nhanh thư mục gốc dưới dạng positional argument:
	// dedupe.exe "D:\\Designs"
	if positionalRoot != "" {
		*root = positionalRoot
	} else if flag.NArg() > 0 {
		*root = flag.Arg(0)
	}

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		log.Fatalf("abs root: %v", err)
	}
	if *batchSize <= 0 {
		log.Fatal("batch-size phải > 0")
	}
	startAll := time.Now()
	log.Printf("[INIT] root=%s distance=%d lsh-bits=%d batch-size=%d dry-run=%v", absRoot, *maxDist, *lshBits, *batchSize, *dryRun)
	if err := validateLSHRecall(*lshBits, *maxDist); err != nil {
		log.Fatal(err)
	}
	log.Printf("[VALIDATE] LSH recall rule passed")

	logFile, err := os.Create(*logPath)
	if err != nil {
		log.Fatalf("tạo log: %v", err)
	}
	defer logFile.Close()

	ts := time.Now().Format(time.RFC3339)
	numBands := 64 / *lshBits
	fmt.Fprintf(logFile, "=== Dedupe design log %s ===\n", ts)
	fmt.Fprintf(logFile, "root=%s max_hamming=%d lsh_bits_per_band=%d lsh_bands=%d batch_size=%d dry_run=%v\n\n",
		absRoot, *maxDist, *lshBits, numBands, *batchSize, *dryRun)

	paths := scanImagePaths(absRoot)
	if len(paths) == 0 {
		log.Println("Không có ảnh trong thư mục gốc.")
		fmt.Fprintln(logFile, "Không có ảnh trong thư mục gốc.")
		return
	}
	log.Printf("[PHASE 1] total input images=%d", len(paths))
	fmt.Fprintf(logFile, "== PHASE 1 (batch local dedupe): total images=%d ==\n", len(paths))

	totalDeletedP1 := 0
	totalGroupsP1 := 0
	totalDecodeErrP1 := 0

	batchCount := (len(paths) + *batchSize - 1) / *batchSize
	for bi := 0; bi < batchCount; bi++ {
		start := bi * *batchSize
		end := start + *batchSize
		if end > len(paths) {
			end = len(paths)
		}
		batchPaths := paths[start:end]
		log.Printf("[PHASE 1][BATCH %d/%d] paths=%d", bi+1, batchCount, len(batchPaths))
		fmt.Fprintf(logFile, "\n[PHASE 1][BATCH %d/%d] paths=%d\n", bi+1, batchCount, len(batchPaths))

		recs, decodeErrs := processImagePaths(batchPaths, fmt.Sprintf("[PHASE 1][BATCH %d/%d][HASH]", bi+1, batchCount))
		totalDecodeErrP1 += decodeErrs
		dStats := dedupeAndDelete(recs, *lshBits, *maxDist, *dryRun, logFile, fmt.Sprintf("PHASE 1 / BATCH %d", bi+1))
		totalDeletedP1 += dStats.Deleted
		totalGroupsP1 += dStats.Groups
		if shouldLogProgress(bi+1, batchCount, 1) {
			log.Printf("[PHASE 1] Progress batch: %d/%d (%.1f%%)", bi+1, batchCount, percent(bi+1, batchCount))
		}
	}

	removedEmptyP1, err := removeEmptyDirs(absRoot, *dryRun)
	if err != nil {
		log.Printf("[CLEANUP PHASE 1] error: %v", err)
		fmt.Fprintf(logFile, "[CLEANUP PHASE 1] error: %v\n", err)
	}
	log.Printf("[CLEANUP PHASE 1] empty dirs removed=%d", removedEmptyP1)
	fmt.Fprintf(logFile, "[CLEANUP PHASE 1] empty dirs removed=%d\n", removedEmptyP1)
	fmt.Fprintf(logFile, "[PHASE 1 SUMMARY] groups=%d deleted=%d decode_errors=%d\n", totalGroupsP1, totalDeletedP1, totalDecodeErrP1)

	log.Printf("[PHASE 2] Rescan survivors and dedupe globally...")
	phase2Paths := scanImagePaths(absRoot)
	fmt.Fprintf(logFile, "\n== PHASE 2 (global merge): total images=%d ==\n", len(phase2Paths))
	recsP2, decodeErrP2 := processImagePaths(phase2Paths, "[PHASE 2][HASH]")
	dStatsP2 := dedupeAndDelete(recsP2, *lshBits, *maxDist, *dryRun, logFile, "PHASE 2 / GLOBAL")
	removedEmptyP2, err := removeEmptyDirs(absRoot, *dryRun)
	if err != nil {
		log.Printf("[CLEANUP PHASE 2] error: %v", err)
		fmt.Fprintf(logFile, "[CLEANUP PHASE 2] error: %v\n", err)
	}
	log.Printf("[CLEANUP PHASE 2] empty dirs removed=%d", removedEmptyP2)
	fmt.Fprintf(logFile, "[CLEANUP PHASE 2] empty dirs removed=%d\n", removedEmptyP2)

	totalDeleted := totalDeletedP1 + dStatsP2.Deleted
	totalGroups := totalGroupsP1 + dStatsP2.Groups
	totalDecodeErr := totalDecodeErrP1 + decodeErrP2
	fmt.Fprintf(logFile, "\n--- FINAL SUMMARY: groups=%d deleted=%d decode_errors=%d mode=%s ---\n",
		totalGroups, totalDeleted, totalDecodeErr, map[bool]string{true: "dry-run", false: "delete"}[*dryRun])

	log.Printf("[DONE] Elapsed=%s, log=%s", time.Since(startAll).Round(time.Millisecond), *logPath)
}

func scanImagePaths(root string) []string {
	log.Printf("[SCAN] Discover image files...")
	var paths []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !imageExts[ext] {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	log.Printf("[SCAN] Found %d images", len(paths))
	return paths
}

func processImagePaths(paths []string, progressPrefix string) ([]*imageRecord, int) {
	out := make([]*imageRecord, 0, len(paths))
	if len(paths) == 0 {
		return out, 0
	}
	log.Printf("%s Decode + normalize + hash...", progressPrefix)
	t0 := time.Now()
	decodeErrs := 0
	for i, path := range paths {
		r := processImage(path)
		if r.Err != nil {
			decodeErrs++
			log.Printf("skip %s: %v", r.Path, r.Err)
			continue
		}
		out = append(out, r)
		if shouldLogProgress(i+1, len(paths), progressStep) {
			log.Printf("%s Progress: %d/%d (%.1f%%)", progressPrefix, i+1, len(paths), percent(i+1, len(paths)))
		}
	}
	log.Printf("%s Done in %s, valid=%d, decode_errors=%d", progressPrefix, time.Since(t0).Round(time.Millisecond), len(out), decodeErrs)
	return out, decodeErrs
}

type dedupeStats struct {
	Groups  int
	Deleted int
}

func dedupeAndDelete(records []*imageRecord, lshBits, maxDist int, dryRun bool, logFile io.Writer, phaseName string) dedupeStats {
	if len(records) == 0 {
		fmt.Fprintf(logFile, "[%s] no valid images\n", phaseName)
		return dedupeStats{}
	}
	hashes := make([]uint64, len(records))
	for i, r := range records {
		hashes[i] = r.Hash.GetHash()
	}

	log.Printf("[%s][INDEX 1/3] Build LSH buckets + BK-tree roots...", phaseName)
	t0 := time.Now()
	roots := buildLSHRoots(hashes, lshBits)
	log.Printf("[%s][INDEX 1/3] Done: buckets=%d, elapsed=%s", phaseName, len(roots), time.Since(t0).Round(time.Millisecond))

	parent := make([]int, len(records))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}

	log.Printf("[%s][INDEX 2/3] Search neighbors with BK-tree in each LSH bucket...", phaseName)
	t1 := time.Now()
	unionNeighborsLSHBK(hashes, lshBits, maxDist, roots, union)
	log.Printf("[%s][INDEX 2/3] Done: elapsed=%s", phaseName, time.Since(t1).Round(time.Millisecond))

	groups := make(map[int][]int)
	for i := range records {
		r := find(i)
		groups[r] = append(groups[r], i)
	}
	log.Printf("[%s][INDEX 3/3] Done: groups=%d", phaseName, len(groups))

	dupGroups := make([][]int, 0, len(groups))
	for _, idxs := range groups {
		if len(idxs) > 1 {
			dupGroups = append(dupGroups, idxs)
		}
	}
	log.Printf("[%s][DELETE] Duplicate groups=%d", phaseName, len(dupGroups))
	fmt.Fprintf(logFile, "[%s] duplicate_groups=%d\n", phaseName, len(dupGroups))

	deleted := 0
	for gi, idxs := range dupGroups {
		sort.Slice(idxs, func(a, b int) bool {
			pa := records[idxs[a]].Pixels
			pb := records[idxs[b]].Pixels
			if pa != pb {
				return pa > pb
			}
			return records[idxs[a]].Path < records[idxs[b]].Path
		})
		keep := idxs[0]
		rest := idxs[1:]
		fmt.Fprintf(logFile, "[%s] GROUP (%d ảnh tương tự):\n", phaseName, len(idxs))
		fmt.Fprintf(logFile, "  KEEP: %s (%dx%d)\n", records[keep].Path, records[keep].Width, records[keep].Height)
		for _, ri := range rest {
			fmt.Fprintf(logFile, "  DEL:  %s (%dx%d)\n", records[ri].Path, records[ri].Width, records[ri].Height)
			if !dryRun {
				if err := os.Remove(records[ri].Path); err != nil {
					fmt.Fprintf(logFile, "  ERROR remove: %v\n", err)
					log.Printf("xóa %s: %v", records[ri].Path, err)
				} else {
					deleted++
				}
			} else {
				deleted++
			}
		}
		fmt.Fprintln(logFile)
		if shouldLogProgress(gi+1, len(dupGroups), 20) {
			log.Printf("[%s][DELETE] Progress groups: %d/%d (%.1f%%)", phaseName, gi+1, len(dupGroups), percent(gi+1, len(dupGroups)))
		}
	}
	fmt.Fprintf(logFile, "[%s] summary: groups=%d deleted=%d mode=%s\n", phaseName, len(dupGroups), deleted, map[bool]string{true: "dry-run", false: "delete"}[dryRun])
	return dedupeStats{Groups: len(dupGroups), Deleted: deleted}
}

func removeEmptyDirs(root string, dryRun bool) (int, error) {
	var dirs []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	removed := 0
	for _, dir := range dirs {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		if len(ents) != 0 {
			continue
		}
		if dryRun {
			removed++
			continue
		}
		if err := os.Remove(dir); err == nil {
			removed++
		}
	}
	return removed, nil
}

func shouldLogProgress(done, total, step int) bool {
	if total <= 0 {
		return false
	}
	if done == total {
		return true
	}
	if step <= 0 {
		return false
	}
	return done%step == 0
}

func percent(done, total int) float64 {
	if total == 0 {
		return 100
	}
	return float64(done) * 100 / float64(total)
}

// normalizeCLIArgs cho phép truyền positional root ở bất kỳ vị trí nào, ví dụ:
// dedupe.exe "D:\\Designs" -batch-size 50
func normalizeCLIArgs(args []string) ([]string, string) {
	expectsValue := map[string]bool{
		"-root":       true,
		"-distance":   true,
		"-lsh-bits":   true,
		"-batch-size": true,
		"-log":        true,
		"-dry-run":    false,
	}
	out := make([]string, 0, len(args))
	rootArg := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			out = append(out, a)
			// Cờ dạng -name=value đã có value trong chính token.
			if strings.Contains(a, "=") {
				continue
			}
			if expectsValue[a] && i+1 < len(args) {
				out = append(out, args[i+1])
				i++
			}
			continue
		}
		if rootArg == "" {
			rootArg = a
		}
	}
	return out, rootArg
}

func processImage(path string) *imageRecord {
	r := &imageRecord{Path: path}
	f, err := os.Open(path)
	if err != nil {
		r.Err = err
		return r
	}
	defer f.Close()

	img, format, err := decodeImage(f)
	if err != nil {
		r.Err = fmt.Errorf("decode: %w", err)
		return r
	}

	b := img.Bounds()
	r.Width, r.Height = b.Dx(), b.Dy()
	r.Pixels = int64(r.Width) * int64(r.Height)

	norm := normalizeImage(img)
	h, err := goimagehash.PerceptionHash(norm)
	if err != nil {
		r.Err = fmt.Errorf("phash: %w", err)
		return r
	}
	_ = format
	r.Hash = h
	return r
}

func decodeImage(r io.Reader) (image.Image, string, error) {
	// Peek first bytes for format sniff if needed; std Decode handles most.
	return image.Decode(r)
}

// normalizeImage: scale ảnh sao cho cạnh dài nhất = normalizeMaxSide, giữ tỷ lệ (bước "Normalize" trong pipeline).
func normalizeImage(src image.Image) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return src
	}
	maxW, maxH := w, h
	if maxW < maxH {
		maxW, maxH = maxH, maxW
	}
	scale := float64(normalizeMaxSide) / float64(maxW)
	nw := int(float64(w)*scale + 0.5)
	nh := int(float64(h)*scale + 0.5)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

// --- LSH + BK-tree (Hamming) ---

type bkNode struct {
	hash uint64
	id   int
	kids map[int]*bkNode
}

func bkInsert(n *bkNode, hash uint64, id int) *bkNode {
	if n == nil {
		return &bkNode{hash: hash, id: id, kids: make(map[int]*bkNode)}
	}
	d := hamming64(n.hash, hash)
	n.kids[d] = bkInsert(n.kids[d], hash, id)
	return n
}

func bkSearch(n *bkNode, q uint64, r int, out *[]int) {
	if n == nil {
		return
	}
	d := hamming64(n.hash, q)
	if d <= r {
		*out = append(*out, n.id)
	}
	for cd, ch := range n.kids {
		if hammingTriOk(cd, d, r) {
			bkSearch(ch, q, r, out)
		}
	}
}

func hammingTriOk(cd, d, r int) bool {
	x := cd - d
	if x < 0 {
		x = -x
	}
	return x <= r
}

func hamming64(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

func bandSegment(h uint64, bandIdx, bitsPerBand int) uint64 {
	shift := uint(bandIdx * bitsPerBand)
	mask := (uint64(1) << uint(bitsPerBand)) - 1
	return (h >> shift) & mask
}

func lshKey(bandIdx int, seg uint64) uint32 {
	return uint32(bandIdx)<<8 | uint32(seg)
}

func buildLSHRoots(hashes []uint64, bitsPerBand int) map[uint32]*bkNode {
	if 64%bitsPerBand != 0 {
		panic("lsh: bitsPerBand must divide 64")
	}
	numBands := 64 / bitsPerBand
	bucketLists := make(map[uint32][]int)
	for i, h := range hashes {
		for b := 0; b < numBands; b++ {
			seg := bandSegment(h, b, bitsPerBand)
			key := lshKey(b, seg)
			bucketLists[key] = append(bucketLists[key], i)
		}
	}
	roots := make(map[uint32]*bkNode, len(bucketLists))
	for key, ids := range bucketLists {
		var root *bkNode
		for _, idx := range ids {
			root = bkInsert(root, hashes[idx], idx)
		}
		roots[key] = root
	}
	return roots
}

func unionNeighborsLSHBK(
	hashes []uint64,
	bitsPerBand, maxDist int,
	roots map[uint32]*bkNode,
	union func(a, b int),
) {
	n := len(hashes)
	if n == 0 {
		return
	}
	numBands := 64 / bitsPerBand
	stamp := make([]int, n)
	token := 0
	var searchBuf []int
	var cands []int

	for i := 0; i < n; i++ {
		token++
		cands = cands[:0]
		hi := hashes[i]
		for b := 0; b < numBands; b++ {
			seg := bandSegment(hi, b, bitsPerBand)
			key := lshKey(b, seg)
			root := roots[key]
			if root == nil {
				continue
			}
			searchBuf = searchBuf[:0]
			bkSearch(root, hi, maxDist, &searchBuf)
			for _, j := range searchBuf {
				if j == i {
					continue
				}
				if stamp[j] != token {
					stamp[j] = token
					cands = append(cands, j)
				}
			}
		}
		for _, j := range cands {
			if hamming64(hi, hashes[j]) <= maxDist {
				union(i, j)
			}
		}
	}
}

func validateLSHRecall(bitsPerBand, maxDist int) error {
	if bitsPerBand < 1 || bitsPerBand > 16 {
		return fmt.Errorf("lsh-bits phải trong [1,16] và chia hết 64")
	}
	if 64%bitsPerBand != 0 {
		return fmt.Errorf("lsh-bits phải là ước của 64 (1,2,4,8,16)")
	}
	numBands := 64 / bitsPerBand
	if maxDist >= numBands {
		return fmt.Errorf(
			"để LSH không bỏ sót cặp trùng: số band (%d) phải > max Hamming (%d); giảm -distance hoặc giảm -lsh-bits (vd. 1 => 64 band)",
			numBands, maxDist,
		)
	}
	return nil
}
