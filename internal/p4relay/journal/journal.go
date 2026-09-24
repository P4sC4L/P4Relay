package journal

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// Entry est une requete enregistree dans le journal d'activite.
type Entry struct {
	ID         string `json:"id"`
	At         string `json:"at"`
	Alias      string `json:"alias"`
	Provider   string `json:"provider"`
	Target     string `json:"target"`
	Status     int    `json:"status"`
	Stream     bool   `json:"stream"`
	Endpoint   string `json:"endpoint"`
	DurationMs int64  `json:"durationMs"`
}

const (
	// PageSize est le nombre d'entrees par page (et renvoyees par /api/state).
	PageSize = 100
	// DefaultMaxEntries est la capacite du ring buffer en memoire.
	DefaultMaxEntries = 10000
	// DefaultFlushInterval est la periode de persistation asynchrone.
	DefaultFlushInterval = 5 * time.Second
	// filePerm : le journal contient des endpoints et des noms de modeles,
	// il n'a rien a etre lisible par les autres utilisateurs de la machine.
	filePerm = 0o600
)

// Option ajuste la creation d'un Journal (New).
type Option func(*Journal)

// WithMaxEntries fixe la capacite du ring buffer (ignor si n <= 0).
func WithMaxEntries(n int) Option {
	return func(j *Journal) {
		if n > 0 {
			j.maxEntries = n
		}
	}
}

// WithFlushInterval fixe la periode de flush (ignor si d <= 0).
func WithFlushInterval(d time.Duration) Option {
	return func(j *Journal) {
		if d > 0 {
			j.flushInterval = d
		}
	}
}

// Journal conserve les entrees en memoire dans un ring buffer borne, les
// persiste en JSON de facon asynchrone et cumule les statistiques de requetes.
//
// Organisation interne : buf[head] est la plus ancienne entree conservee,
// la plus recente est juste avant head. L'ajout est donc en O(1) et sans
// reallocation, ce qui etait le defaut de l'ancien " append en tete ".
type Journal struct {
	path          string
	maxEntries    int
	flushInterval time.Duration

	mu    sync.Mutex
	buf   []Entry
	head  int // index de l'entree la plus ancienne
	count int // nombre d'entrees valide (<= len(buf))
	dirty bool

	statsMu    sync.Mutex
	requests   int
	successful int
	failed     int
	totalMs    int64

	saveMu   sync.Mutex // serialise les acces au fichier (flush, Clear)
	stopCh   chan struct{}
	stopOnce sync.Once
}

// New cree un journal persiste au chemin donne et demarre la goroutine de
// flush periodique. L'appelant doit fermer le propre : defer j.Stop().
func New(path string, opts ...Option) *Journal {
	j := &Journal{
		path:          path,
		maxEntries:    DefaultMaxEntries,
		flushInterval: DefaultFlushInterval,
		stopCh:        make(chan struct{}),
	}
	for _, opt := range opts {
		opt(j)
	}
	j.buf = make([]Entry, j.maxEntries)
	go j.flushLoop()
	return j
}

// Load recharge les entrees persistees (BOM tolere, fichier illisible ignore).
func (j *Journal) Load() {
	data, err := os.ReadFile(j.path)
	if err != nil || len(data) == 0 {
		return
	}
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}
	var entries []Entry
	if json.Unmarshal(data, &entries) != nil {
		return
	}
	// Le fichier est ecrit du plus recent au plus ancien : on ne garde que
	// les maxEntries premieres (les plus recentes) et on les insere en sens
	// inverse pour retrouver le bon ordre chronologique dans l'anneau.
	keep := len(entries)
	if keep > j.maxEntries {
		keep = j.maxEntries
	}
	j.mu.Lock()
	for i := keep - 1; i >= 0; i-- {
		j.push(entries[i])
	}
	j.mu.Unlock()
	if keep > 0 {
		log.Printf("journal: %d entrées rechargées depuis %s", keep, j.path)
	}
}

// ---------- ecriture ----------

// Record ajoute une entree (la plus recente), met a jour les statistiques et
// marque le journal modifie. La persistation est deleguee au flush periodique.
func (j *Journal) Record(entry Entry) {
	j.mu.Lock()
	j.push(entry)
	j.dirty = true
	j.mu.Unlock()

	j.statsMu.Lock()
	j.requests++
	j.totalMs += entry.DurationMs
	if entry.Status >= 200 && entry.Status < 300 {
		j.successful++
	} else {
		j.failed++
	}
	j.statsMu.Unlock()
}

// push insere en fin d'anneau (la plus recente) et ecrase la plus ancienne si
// la capacite est atteinte. Doit etre appele sous j.mu.
func (j *Journal) push(entry Entry) {
	n := len(j.buf)
	if n == 0 {
		return
	}
	if j.count < n {
		j.buf[(j.head+j.count)%n] = entry
		j.count++
		return
	}
	j.buf[j.head] = entry
	j.head = (j.head + 1) % n
}

// at renvoie l'entree de rang i en partant du plus recent (i = 0 => derniere
// arrivee). Doit etre appele sous j.mu, avec 0 <= i < j.count.
func (j *Journal) at(i int) Entry {
	n := len(j.buf)
	idx := (j.head + j.count - 1 - i + n) % n
	return j.buf[idx]
}

// ---------- lecture ----------

// Snapshot renvoie les limit entrees les plus recentes, reordonnees en ordre
// chronologique (la plus ancienne des limit renvoyees en premier).
func (j *Journal) Snapshot(limit int) []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	if limit <= 0 {
		return []Entry{}
	}
	if limit > j.count {
		limit = j.count
	}
	out := make([]Entry, 0, limit)
	// Les " limit plus recentes " sont at(limit-1) ... at(0).
	for i := limit - 1; i >= 0; i-- {
		out = append(out, j.at(i))
	}
	return out
}

// Page renvoie la page demandee (1-based, la page 1 = les plus recentes) et le
// nombre total d'entrees conservees.
func (j *Journal) Page(page, size int) ([]Entry, int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if size <= 0 {
		return []Entry{}, j.count
	}
	start := 0
	if page > 1 {
		// Garde anti-debordement : au-dela de la derniere page possible,
		// (page-1)*size n'a pas besoin d'etre calcule.
		if page-1 > j.count/size {
			return []Entry{}, j.count
		}
		start = (page - 1) * size
	}
	if start >= j.count {
		return []Entry{}, j.count
	}
	end := start + size
	if end > j.count {
		end = j.count
	}
	out := make([]Entry, 0, end-start)
	for i := start; i < end; i++ {
		out = append(out, j.at(i))
	}
	return out, j.count
}

// Stats renvoie le cumul des requetes traitees.
func (j *Journal) Stats() map[string]any {
	j.statsMu.Lock()
	defer j.statsMu.Unlock()
	return map[string]any{"requests": j.requests, "successful": j.successful,
		"failed": j.failed, "totalMs": j.totalMs}
}

// Clear vide le journal, remet les statistiques a zero et supprime le fichier
// persiste. L'ordre des verrous (saveMu puis mu) est identique a celui de
// flush : un flush deja parti ne peut donc pas recreer le fichier efface.
func (j *Journal) Clear() {
	j.saveMu.Lock()
	defer j.saveMu.Unlock()

	j.mu.Lock()
	j.buf = make([]Entry, j.maxEntries)
	j.head = 0
	j.count = 0
	j.dirty = false
	j.mu.Unlock()

	j.statsMu.Lock()
	j.requests = 0
	j.successful = 0
	j.failed = 0
	j.totalMs = 0
	j.statsMu.Unlock()

	if err := os.Remove(j.path); err != nil && !os.IsNotExist(err) {
		log.Printf("journal: %v", err)
	}
}

// ---------- cycle de vie ----------

// Stop arrete la goroutine de flush et force une derniere persistation.
// Peut etre appelee plusieurs fois sans effet de bord.
func (j *Journal) Stop() {
	j.stopOnce.Do(func() {
		close(j.stopCh)
		j.flush()
	})
}

func (j *Journal) flushLoop() {
	ticker := time.NewTicker(j.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-j.stopCh:
			return
		case <-ticker.C:
			j.flush()
		}
	}
}

// flush ne fait rien si rien n'a change depuis le dernier passage ; sinon il
// copie les donnees sous verrou puis ecrit hors verrou.
func (j *Journal) flush() {
	j.mu.Lock()
	if !j.dirty {
		j.mu.Unlock()
		return
	}
	entries := make([]Entry, 0, j.count)
	for i := 0; i < j.count; i++ {
		entries = append(entries, j.at(i))
	}
	total := j.count
	j.dirty = false
	j.mu.Unlock()

	j.saveMu.Lock()
	defer j.saveMu.Unlock()
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	tmpPath := j.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, filePerm); err != nil {
		log.Printf("journal: %v", err)
		return
	}
	// Renommage atomique : un crash en cours d'ecriture laisse l'ancien
	// journal intact plutot qu'un fichier tronque.
	if err := os.Rename(tmpPath, j.path); err != nil {
		log.Printf("journal: %v", err)
		_ = os.Remove(tmpPath)
		return
	}
	log.Printf("journal: %d entrées conservées dans %s", total, j.path)
}
