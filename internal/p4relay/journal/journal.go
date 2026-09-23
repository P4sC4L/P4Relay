package journal

import (
	"encoding/json"
	"log"
	"os"
	"sync"
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

// PageSize est le nombre d'entrees par page (et renvoyees par /api/state).
const PageSize = 100

// Journal conserve les entrees en memoire, les persiste en JSON et cumule les
// statistiques de requetes.
type Journal struct {
	path string

	mu       sync.Mutex
	logs     []Entry
	logTotal int

	statsMu    sync.Mutex
	requests   int
	successful int
	failed     int
	totalMs    int64
}

// New cree un journal persiste au chemin donne.
func New(path string) *Journal {
	return &Journal{path: path, logs: []Entry{}}
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
	total := len(entries)
	j.mu.Lock()
	j.logs = entries
	j.logTotal = total
	j.mu.Unlock()
	log.Printf("journal: %d entr\u00e9es recharg\u00e9es depuis %s", total, j.path)
}

func (j *Journal) save() {
	j.mu.Lock()
	entries := make([]Entry, len(j.logs))
	copy(entries, j.logs)
	total := j.logTotal
	j.mu.Unlock()
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	if err := os.WriteFile(j.path, data, 0o644); err != nil {
		log.Printf("journal: %v", err)
	}
	log.Printf("journal: %d entr\u00e9es conserv\u00e9es dans %s", total, j.path)
}

// Record ajoute une entree en tete, met a jour les statistiques et persiste.
func (j *Journal) Record(entry Entry) {
	j.mu.Lock()
	j.logs = append([]Entry{entry}, j.logs...)
	j.logTotal++
	j.mu.Unlock()
	j.save()
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

// Snapshot renvoie les limit dernieres entrees dans l'ordre chronologique.
func (j *Journal) Snapshot(limit int) []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	start := 0
	if len(j.logs) > limit {
		start = len(j.logs) - limit
	}
	out := make([]Entry, len(j.logs)-start)
	copy(out, j.logs[start:])
	return out
}

// Page renvoie la page demandee (1-based) et le total.
func (j *Journal) Page(page, size int) ([]Entry, int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	start := (page - 1) * size
	var slice []Entry
	if start < len(j.logs) {
		slice = j.logs[start:]
	}
	return slice, j.logTotal
}

// Stats renvoie le cumul des requetes traitees.
func (j *Journal) Stats() map[string]any {
	j.statsMu.Lock()
	defer j.statsMu.Unlock()
	return map[string]any{"requests": j.requests, "successful": j.successful,
		"failed": j.failed, "totalMs": j.totalMs}
}

// Clear vide le journal et supprime le fichier persiste.
func (j *Journal) Clear() {
	j.mu.Lock()
	j.logs = []Entry{}
	j.logTotal = 0
	j.mu.Unlock()
	os.Remove(j.path)
}
