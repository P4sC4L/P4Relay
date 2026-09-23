package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

func (g *gateway) journalPath() string {
	return filepath.Join(g.dataDir, "journal.log")
}

func (g *gateway) loadJournal() {
	data, err := os.ReadFile(g.journalPath())
	if err != nil || len(data) == 0 {
		return
	}
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}
	var entries []logEntry
	if json.Unmarshal(data, &entries) != nil {
		return
	}
	total := len(entries)
	g.logsMu.Lock()
	g.logs = entries
	g.logTotal = total
	g.logsMu.Unlock()
	log.Printf("journal: %d entrées rechargées depuis %s", total, g.journalPath())
}

func (g *gateway) saveJournal() {
	g.logsMu.Lock()
	entries := make([]logEntry, len(g.logs))
	copy(entries, g.logs)
	total := g.logTotal
	g.logsMu.Unlock()
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	if err := os.WriteFile(g.journalPath(), data, 0o644); err != nil {
		log.Printf("journal: %v", err)
	}
	log.Printf("journal: %d entrées conservées dans %s", total, g.journalPath())
}

func (g *gateway) record(entry logEntry) {
	g.logsMu.Lock()
	g.logs = append([]logEntry{entry}, g.logs...)
	g.logTotal++
	g.logsMu.Unlock()
	g.saveJournal()
	g.statsMu.Lock()
	g.requests++
	g.totalMs += entry.DurationMs
	if entry.Status >= 200 && entry.Status < 300 {
		g.successful++
	} else {
		g.failed++
	}
	g.statsMu.Unlock()
}

func (g *gateway) clearLogs() {
	g.logsMu.Lock()
	g.logs = []logEntry{}
	g.logTotal = 0
	g.logsMu.Unlock()
	os.Remove(g.journalPath())
}
