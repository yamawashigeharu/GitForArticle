// 稿本打包：把“每个子文件夹是一版”的目录直接打成 git bundle，不需要安装 Git。
package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

const version = "v0.1.0-rev1"

/* ---------------- folder scanning ---------------- */

var ignoredNames = map[string]bool{"thumbs.db": true, "desktop.ini": true, ".ds_store": true, ".git": true, "$recycle.bin": true}

func ignored(name string) bool {
	l := strings.ToLower(name)
	return ignoredNames[l] || strings.HasPrefix(name, "~$") || strings.HasPrefix(name, ".~") || strings.HasSuffix(l, ".tmp")
}

type snapshot struct {
	name  string
	dir   string
	time  time.Time // newest file modification time inside
	files int
}

type branch struct {
	name  string
	snaps []*snapshot
}

// scanSnapshot returns newest mtime and file count of a folder tree.
func scanSnapshot(dir string) (time.Time, int, error) {
	var newest time.Time
	n := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir && ignored(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			n++
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		return nil
	})
	if n == 0 {
		info, e := os.Stat(dir)
		if e == nil {
			newest = info.ModTime()
		}
	}
	return newest, n, err
}

func listDir(dir string) (dirs []os.DirEntry, files int, err error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	for _, e := range ents {
		if ignored(e.Name()) {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, e)
		} else if e.Type().IsRegular() {
			files++
		}
	}
	return dirs, files, nil
}

// naturalLess compares strings with digit runs compared numerically.
func naturalLess(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		if unicode.IsDigit(ra[i]) && unicode.IsDigit(rb[j]) {
			si := i
			for i < len(ra) && unicode.IsDigit(ra[i]) {
				i++
			}
			sj := j
			for j < len(rb) && unicode.IsDigit(rb[j]) {
				j++
			}
			na := strings.TrimLeft(string(ra[si:i]), "0")
			nb := strings.TrimLeft(string(rb[sj:j]), "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if ra[i] != rb[j] {
			return ra[i] < rb[j]
		}
		i++
		j++
	}
	return len(ra)-i < len(rb)-j
}

type plan struct {
	root     string
	name     string
	branches []*branch
	byName   bool
	flat     bool
	skipped  int // loose files directly in root
}

func sortSnaps(s []*snapshot, byName bool) {
	sort.SliceStable(s, func(i, j int) bool {
		if byName {
			return naturalLess(s[i].name, s[j].name)
		}
		if !s[i].time.Equal(s[j].time) {
			return s[i].time.Before(s[j].time)
		}
		return naturalLess(s[i].name, s[j].name)
	})
}

func makePlan(root string, flat, byName bool) (*plan, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("找不到这个文件夹：%s", root)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("拖进来的是文件，不是文件夹：%s", root)
	}
	p := &plan{root: root, name: filepath.Base(root), flat: flat, byName: byName}
	children, loose, err := listDir(root)
	if err != nil {
		return nil, err
	}
	p.skipped = loose
	main := &branch{name: p.name}
	for _, c := range children {
		cdir := filepath.Join(root, c.Name())
		sub, files, err := listDir(cdir)
		if err != nil {
			return nil, err
		}
		if !flat && files == 0 && len(sub) > 0 {
			// a folder that only contains folders: one 稿 (branch), its subfolders are the versions
			b := &branch{name: c.Name()}
			for _, s := range sub {
				sdir := filepath.Join(cdir, s.Name())
				t, n, err := scanSnapshot(sdir)
				if err != nil {
					return nil, err
				}
				b.snaps = append(b.snaps, &snapshot{name: s.Name(), dir: sdir, time: t, files: n})
			}
			sortSnaps(b.snaps, byName)
			p.branches = append(p.branches, b)
			continue
		}
		t, n, err := scanSnapshot(cdir)
		if err != nil {
			return nil, err
		}
		main.snaps = append(main.snaps, &snapshot{name: c.Name(), dir: cdir, time: t, files: n})
	}
	if len(main.snaps) > 0 {
		sortSnaps(main.snaps, byName)
		p.branches = append([]*branch{main}, p.branches...)
	}
	if len(p.branches) == 0 {
		return nil, errors.New("这个文件夹里没有子文件夹。请把每一版放在单独的子文件夹里，再拖整个上层文件夹进来。")
	}
	return p, nil
}

/* ---------------- git objects + pack ---------------- */

type packWriter struct {
	tmp   *os.File
	count uint32
	seen  map[[20]byte]bool
}

func (w *packWriter) put(kind string, typ byte, data []byte) [20]byte {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(data))
	h.Write(data)
	var id [20]byte
	copy(id[:], h.Sum(nil))
	if w.seen[id] {
		return id
	}
	w.seen[id] = true
	// object header: type and size varint
	size := uint64(len(data))
	var hdr []byte
	c := byte(typ<<4) | byte(size&15)
	size >>= 4
	for size > 0 {
		hdr = append(hdr, c|0x80)
		c = byte(size & 0x7f)
		size >>= 7
	}
	hdr = append(hdr, c)
	w.tmp.Write(hdr)
	zw, _ := zlib.NewWriterLevel(w.tmp, zlib.BestSpeed)
	zw.Write(data)
	zw.Close()
	w.count++
	return id
}

type entry struct {
	name  string
	isDir bool
	id    [20]byte
}

func gitNameLess(a, b entry) bool {
	an, bn := a.name, b.name
	if a.isDir {
		an += "/"
	}
	if b.isDir {
		bn += "/"
	}
	return an < bn
}

// writeTree stores a folder as a git tree; returns ok=false for folders with no files.
func (w *packWriter) writeTree(dir, rel string, stats *stats) ([20]byte, bool, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return [20]byte{}, false, err
	}
	var list []entry
	for _, e := range ents {
		if ignored(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			id, ok, err := w.writeTree(p, rel+e.Name()+"/", stats)
			if err != nil {
				return id, false, err
			}
			if ok {
				list = append(list, entry{e.Name(), true, id})
			}
		} else if e.Type().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return [20]byte{}, false, fmt.Errorf("读不了文件（是否正在被 Word 打开？）：%s", p)
			}
			before := w.count
			id := w.put("blob", 3, data)
			if w.count > before {
				stats.newBytes += int64(len(data))
			}
			stats.files[rel+e.Name()] = id
			list = append(list, entry{e.Name(), false, id})
		}
	}
	if len(list) == 0 {
		return [20]byte{}, false, nil
	}
	sort.Slice(list, func(i, j int) bool { return gitNameLess(list[i], list[j]) })
	var buf bytes.Buffer
	for _, e := range list {
		if e.isDir {
			buf.WriteString("40000 ")
		} else {
			buf.WriteString("100644 ")
		}
		buf.WriteString(e.name)
		buf.WriteByte(0)
		buf.Write(e.id[:])
	}
	return w.put("tree", 2, buf.Bytes()), true, nil
}

type stats struct {
	files    map[string][20]byte // path -> blob id in this version
	newBytes int64               // bytes of content not seen in any earlier version
}

// describe compares this version's files with the previous version of the same 稿.
func describe(prev, cur map[string][20]byte, newBytes int64) string {
	var add, mod, del int
	for p, id := range cur {
		if old, ok := prev[p]; !ok {
			add++
		} else if old != id {
			mod++
		}
	}
	for p := range prev {
		if _, ok := cur[p]; !ok {
			del++
		}
	}
	var parts []string
	if add > 0 {
		parts = append(parts, fmt.Sprintf("新增 %d", add))
	}
	if mod > 0 {
		parts = append(parts, fmt.Sprintf("修改 %d", mod))
	}
	if del > 0 {
		parts = append(parts, fmt.Sprintf("删除 %d", del))
	}
	if len(parts) == 0 {
		return "与上一版相同"
	}
	if newBytes == 0 {
		return strings.Join(parts, " · ")
	}
	return strings.Join(parts, " · ") + "，" + human(newBytes)
}

func sigTime(t time.Time) string {
	_, off := t.Zone()
	sign := '+'
	if off < 0 {
		sign = '-'
		off = -off
	}
	return fmt.Sprintf("%d %c%02d%02d", t.Unix(), sign, off/3600, (off%3600)/60)
}

// refName makes a folder name usable as a git branch name.
func refName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r):
			b.WriteRune('-')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	out = strings.ReplaceAll(out, "@{", "@-")
	out = strings.Trim(out, "./-")
	out = strings.TrimSuffix(out, ".lock")
	if out == "" || out == "@" || out == "HEAD" {
		out = "main"
	}
	return out
}

type result struct {
	path     string
	commits  int
	objects  uint32
	size     int64
	branches []string
}

func build(p *plan, author string, out string, progress func(string)) (*result, error) {
	tmp, err := os.CreateTemp(filepath.Dir(out), ".gaoben-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("没法在输出位置写文件：%v", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	w := &packWriter{tmp: tmp, seen: map[[20]byte]bool{}}
	email := strings.ToLower(refName(author)) + "@localhost"
	type ref struct {
		name string
		id   [20]byte
		time time.Time
	}
	var refs []ref
	used := map[string]int{}
	commits := 0
	for _, b := range p.branches {
		var parent *[20]byte
		var last time.Time
		prevFiles := map[string][20]byte{}
		for i, s := range b.snaps {
			st := &stats{files: map[string][20]byte{}}
			tree, ok, err := w.writeTree(s.dir, "", st)
			if err != nil {
				return nil, err
			}
			if !ok {
				var buf bytes.Buffer
				tree = w.put("tree", 2, buf.Bytes())
			}
			t := s.time
			if !last.IsZero() && t.Before(last) {
				t = last.Add(time.Second) // keep history moving forward when sorted by name
			}
			last = t
			var c bytes.Buffer
			fmt.Fprintf(&c, "tree %s\n", hex.EncodeToString(tree[:]))
			if parent != nil {
				fmt.Fprintf(&c, "parent %s\n", hex.EncodeToString(parent[:]))
			}
			sig := fmt.Sprintf("%s <%s> %s", author, email, sigTime(t))
			fmt.Fprintf(&c, "author %s\ncommitter %s\n\n%s\n\n%d\n", sig, sig, s.name, i+1)
			id := w.put("commit", 1, c.Bytes())
			parent = &id
			commits++
			progress(fmt.Sprintf("  %s / %d. %s  （%s）", b.name, i+1, s.name, describe(prevFiles, st.files, st.newBytes)))
			prevFiles = st.files
		}
		if parent != nil {
			n := refName(b.name)
			used[n]++
			if used[n] > 1 {
				n = fmt.Sprintf("%s-%d", n, used[n])
			}
			refs = append(refs, ref{n, *parent, last})
		}
	}
	// HEAD: the 稿 with the newest version
	head := 0
	for i, r := range refs {
		if r.time.After(refs[head].time) {
			head = i
		}
	}
	// assemble bundle: header + pack header + objects + sha1 trailer
	final, err := os.CreateTemp(filepath.Dir(out), ".gaoben-*.bundle")
	if err != nil {
		return nil, err
	}
	finalName := final.Name()
	ok := false
	defer func() {
		if !ok {
			final.Close()
			os.Remove(finalName)
		}
	}()
	bw := bufio.NewWriterSize(final, 1<<20)
	bw.WriteString("# v2 git bundle\n")
	var names []string
	for _, r := range refs {
		fmt.Fprintf(bw, "%s refs/heads/%s\n", hex.EncodeToString(r.id[:]), r.name)
		names = append(names, r.name)
	}
	fmt.Fprintf(bw, "%s HEAD\n\n", hex.EncodeToString(refs[head].id[:]))
	ph := sha1.New()
	mw := io.MultiWriter(bw, ph)
	var hdr [12]byte
	copy(hdr[:], "PACK")
	binary.BigEndian.PutUint32(hdr[4:], 2)
	binary.BigEndian.PutUint32(hdr[8:], w.count)
	mw.Write(hdr[:])
	if _, err := tmp.Seek(0, 0); err != nil {
		return nil, err
	}
	if _, err := io.Copy(mw, tmp); err != nil {
		return nil, err
	}
	bw.Write(ph.Sum(nil))
	if err := bw.Flush(); err != nil {
		return nil, err
	}
	if err := final.Close(); err != nil {
		return nil, err
	}
	os.Remove(out)
	if err := os.Rename(finalName, out); err != nil {
		return nil, fmt.Errorf("没法写入 %s（是否正在被其他程序打开？）：%v", out, err)
	}
	ok = true
	fi, _ := os.Stat(out)
	return &result{path: out, commits: commits, objects: w.count, size: fi.Size(), branches: names}, nil
}

func human(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.0f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
}

/* ---------------- console UI ---------------- */

func printPlan(p *plan) {
	fmt.Printf("\n稿本打包 %s\n仓库：%s\n", version, p.name)
	order := "按最后修改时间，从早到晚"
	if p.byName {
		order = "按文件夹名称"
	}
	fmt.Printf("排序：%s\n", order)
	if p.skipped > 0 {
		fmt.Printf("提示：%s 里直接放着 %d 个文件，不属于任何一版，会被忽略。\n", p.name, p.skipped)
	}
	for _, b := range p.branches {
		fmt.Printf("\n【%s】 %d 版\n", b.name, len(b.snaps))
		for i, s := range b.snaps {
			fmt.Printf("  %2d. %-24s %s  %d 个文件\n", i+1, s.name, s.time.Format("2006-01-02 15:04"), s.files)
		}
	}
	fmt.Println()
}

func defaultAuthor() string {
	for _, k := range []string{"GAOBEN_AUTHOR", "USERNAME", "USER"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "author"
}

func main() {
	yes := flag.Bool("y", false, "不询问，直接生成")
	byName := flag.Bool("name", false, "按文件夹名称排序")
	flat := flag.Bool("flat", false, "所有子文件夹都当作同一稿的各版")
	outDir := flag.String("o", "", "bundle 输出目录（默认放在拖入文件夹的旁边）")
	author := flag.String("author", defaultAuthor(), "提交作者名")
	showVersion := flag.Bool("version", false, "显示版本号")
	flag.Parse()
	if *showVersion {
		fmt.Println("稿本打包 " + version)
		return
	}
	setupConsole()
	interactive := !*yes

	root := ""
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	} else {
		fmt.Println("稿本打包 " + version)
		fmt.Println("输入（或粘贴、拖入）要打包的文件夹路径，然后按回车：")
		root = readLine()
	}
	root = strings.Trim(strings.TrimSpace(root), "\"'")
	if root == "" {
		fail("没有收到文件夹。", interactive)
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	root = strings.TrimRight(root, `\/`)
	if len(root) == 2 && root[1] == ':' {
		fail("请拖一个文件夹进来，不要拖整个磁盘。", interactive)
	}

	isFlat, isByName := *flat, *byName
	for {
		p, err := makePlan(root, isFlat, isByName)
		if err != nil {
			fail(err.Error(), interactive)
		}
		printPlan(p)
		if interactive {
			fmt.Println("回车：开始生成　　n：改为按文件夹名称排序　　t：改回按时间排序")
			fmt.Println("f：所有子文件夹都算同一稿　　a：自动识别分稿　　q：退出")
			fmt.Print("> ")
			switch strings.ToLower(strings.TrimSpace(readLine())) {
			case "":
			case "n":
				isByName = true
				continue
			case "t":
				isByName = false
				continue
			case "f":
				isFlat = true
				continue
			case "a":
				isFlat = false
				continue
			case "q":
				return
			default:
				continue
			}
		}
		dir := filepath.Dir(root)
		if *outDir != "" {
			dir = *outDir
		}
		out := filepath.Join(dir, p.name+".bundle")
		fmt.Println("正在生成……")
		res, err := build(p, *author, out, func(s string) { fmt.Println(s) })
		if err != nil {
			fail(err.Error(), interactive)
		}
		fmt.Printf("\n完成：%s\n%d 次提交，%d 稿（%s），%s\n", res.path, res.commits, len(res.branches), strings.Join(res.branches, "、"), human(res.size))
		fmt.Println("把这个文件发到手机，在“稿本书架”里导入即可。")
		if interactive {
			fmt.Print("\n按回车关闭")
			readLine()
		}
		return
	}
}

func fail(msg string, wait bool) {
	fmt.Println("\n出错了：" + msg)
	if wait {
		fmt.Print("\n按回车关闭")
		readLine()
	}
	os.Exit(1)
}
