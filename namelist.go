package dnsredir

import (
	"bufio"
	"fmt"
	"github.com/coredns/coredns/plugin"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type stringSet map[string]struct{}
// uint16 used to store two ASCII characters
type domainSet map[uint16]stringSet

func (s *stringSet) Add(str string) {
	(*s)[str] = struct{}{}
}

func (s *stringSet) Contains(str string) bool {
	if s == nil {
		return false
	}
	_, ok := (*s)[str]
	return ok
}

func (d domainSet) String() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%T[", d))

	var i uint64
	n := d.Len()
	for _, s := range d {
		for name := range s {
			sb.WriteString(name)
			if i++; i != n {
				sb.WriteString(", ")
			}
		}
	}
	sb.WriteString("]")

	return sb.String()
}

// Return total number of domains in the domain set
func (d *domainSet) Len() uint64 {
	var n uint64
	for _, s := range *d {
		n += uint64(len(s))
	}
	return n
}

func domainToIndex(str string) uint16 {
	n := len(str)
	if n == 0 {
		panic(fmt.Sprintf("Unexpected empty string?!"))
	}
	// Since we use two ASCII characters to present index
	//	Insufficient length will padded with '-'
	//	Since a valid domain segment will never begin with '-'
	//	So it can maintain balance between buckets
	if n == 1 {
		return (uint16('-') << 8) | uint16(str[0])
	}
	return uint16((str[0] << 8) | str[1])
}

// Return true if name added successfully, false otherwise
func (d *domainSet) Add(str string) bool {
	// To reduce memory, we don't use full qualified name
	if name, ok := stringToDomain(str); ok {
		// To speed up name lookup, we utilized two-way hash
		// The first one is the first two ASCII characters of the domain name
		// The second one is the real domain set
		// Which works somewhat like ordinary English dictionary lookup
		s := (*d)[domainToIndex(name)]
		if s == nil {
			// MT-Unsafe: Initialize real domain set on demand
			s = make(stringSet)
			(*d)[domainToIndex(name)] = s
		}
		s.Add(name)
		return true
	}
	return false
}

// for loop will exit in advance if f() return error
func (d *domainSet) ForEachDomain(f func(name string) error) error {
	for _, s := range *d {
		for name := range s {
			if err := f(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// Assume `child' is lower cased and without trailing dot
func (d *domainSet) Match(child string) bool {
	if len(child) == 0 {
		panic(fmt.Sprintf("Why child is an empty string?!"))
	}

	for {
		s := (*d)[domainToIndex(child)]
		// Fast lookup for a full match
		if s.Contains(child) {
			return true
		}

		// Fallback to iterate the whole set
		for parent := range s {
			if plugin.Name(parent).Matches(child) {
				return true
			}
		}

		i := strings.Index(child, ".")
		if i <= 0 {
			break
		}
		child = child[i+1:]
	}

	return false
}

type Nameitem struct {
	sync.RWMutex

	// Domain name set for lookups
	names domainSet

	path string
	mtime time.Time
	size int64
}

func NewNameitemsWithPaths(paths []string) []*Nameitem {
	items := make([]*Nameitem, len(paths))
	for i, path := range paths {
		items[i] = &Nameitem{
			path: path,
		}
	}
	return items
}

type Namelist struct {
	// List of name items
	items []*Nameitem

	// Time between two reload of a name item
	// All name items shared the same reload duration
	reload time.Duration

	stopReload chan struct{}
}

// Assume `child' is lower cased and without trailing dot
func (n *Namelist) Match(child string) bool {
	for _, item := range n.items {
		item.RLock()
		if item.names.Match(child) {
			item.RUnlock()
			return true
		}
		item.RUnlock()
	}
	return false
}

// MT-Unsafe
func (n *Namelist) periodicUpdate() {
	// Kick off initial name list content population
	n.parseNamelist()

	if n.reload != 0 {
		go func() {
			ticker := time.NewTicker(n.reload)
			for {
				select {
				case <-n.stopReload:
					return
				case <-ticker.C:
					n.parseNamelist()
				}
			}
		}()
	}
}

func (n *Namelist) parseNamelist() {
	for _, item := range n.items {
		n.parseNamelistCore(item)
	}
}

func (n *Namelist) parseNamelistCore(item *Nameitem) {
	file, err := os.Open(item.path)
	if err != nil {
		if os.IsNotExist(err) {
			// File not exist already reported at setup stage
			log.Debugf("%v", err)
		} else {
			log.Warningf("%v", err)
		}
		return
	}
	defer Close(file)

	stat, err := file.Stat()
	if err == nil {
		item.RLock()
		mtime := item.mtime
		size := item.size
		item.RUnlock()

		if stat.ModTime() == mtime && stat.Size() == size {
			return
		}
	} else {
		// Proceed parsing anyway
		log.Warningf("%v", err)
	}

	t1 := time.Now()
	names, totalLines := n.parse(file)
	t2 := time.Since(t1)
	log.Debugf("Parsed %v  time spent: %v name added: %v / %v",
		file.Name(), t2, names.Len(), totalLines)

	item.Lock()
	item.names = names
	item.mtime = stat.ModTime()
	item.size = stat.Size()
	item.Unlock()
}

func (n *Namelist) parse(r io.Reader) (domainSet, uint64) {
	names := make(domainSet)

	var totalLines uint64
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		totalLines++

		line := scanner.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}

		f := strings.Split(line, "/")
		if len(f) != 3 {
			// Treat the whole line as a domain name
			_ = names.Add(line)
			continue
		}

		// Format: server=/<domain>/<?>
		if f[0] != "server=" {
			continue
		}

		// Don't check f[2], see: http://manpages.ubuntu.com/manpages/bionic/man8/dnsmasq.8.html
		// Thus server=/<domain>/<ip>, server=/<domain>/, server=/<domain>/# won't be honored

		if !names.Add(f[1]) {
			log.Warningf("%q isn't a domain name", f[1])
		}
	}

	return names, totalLines
}

