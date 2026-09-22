package skills

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"digital.vasic.concurrency/pkg/safe"
	"github.com/sirupsen/logrus"
)

// Registry manages the collection of available skills.
//
// Concurrent-safe by construction (CONST-029):
//   - Per-collection safe.Store fields hold the skill index. Reads are
//     lock-free.
//   - writeMu serialises the rare multi-map writes (Load, RegisterSkill,
//     Remove) so concurrent writers cannot tangle the four indexes.
//     Readers do not take writeMu — momentary cross-map inconsistency
//     during a Load is acceptable for the registry's read patterns
//     (Search dedupes; Get/GetByCategory/GetByTrigger each touch one
//     index).
//   - stats is an atomic.Pointer[RegistryStats]; updateStats builds a
//     fresh value and Stores it.
type Registry struct {
	writeMu    sync.Mutex // serialises Load, Remove, RegisterSkill
	skills     *safe.Store[string, *Skill]
	byCategory *safe.Store[string, []*Skill]
	byTrigger  *safe.Store[string, []*Skill]
	categories *safe.Store[string, *SkillCategory]
	parser     *Parser
	config     *SkillConfig
	stats      atomic.Pointer[RegistryStats]
	// lastLoadFailures is the T-P6.04 load-report: every malformed skill
	// file the most recent Load/LoadFromPath surfaced. Protected by
	// writeMu (written only from those two callers, which already hold it).
	lastLoadFailures atomic.Pointer[[]ParseFailure]
	watcher          *DirectoryWatcher
	log        *logrus.Logger
}

// DirectoryWatcher watches for skill file changes.
type DirectoryWatcher struct {
	dir      string
	interval time.Duration
	stop     chan struct{}
	callback func()
}

// NewRegistry creates a new skill registry.
func NewRegistry(config *SkillConfig) *Registry {
	if config == nil {
		config = DefaultSkillConfig()
	}

	r := &Registry{
		skills:     safe.NewStore[string, *Skill](),
		byCategory: safe.NewStore[string, []*Skill](),
		byTrigger:  safe.NewStore[string, []*Skill](),
		categories: safe.NewStore[string, *SkillCategory](),
		parser:     NewParser(),
		config:     config,
		log:        logrus.New(),
	}
	r.stats.Store(&RegistryStats{SkillsByCategory: make(map[string]int)})
	return r
}

// SetLogger sets the logger for the registry.
func (r *Registry) SetLogger(log *logrus.Logger) {
	r.log = log
}

// Load loads all skills from the configured directory.
func (r *Registry) Load(ctx context.Context) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	r.log.WithField("directory", r.config.SkillsDirectory).Info("Loading skills")

	skills, failures, err := r.parser.ParseDirectory(r.config.SkillsDirectory)
	if err != nil {
		return fmt.Errorf("failed to parse skills directory: %w", err)
	}
	r.recordLoadFailuresLocked(failures)

	// Clear existing data
	r.skills.Clear()
	r.byCategory.Clear()
	r.byTrigger.Clear()

	// Register each skill
	for _, skill := range skills {
		r.registerSkillLocked(skill)
	}

	// Update stats
	r.updateStatsLocked()

	r.log.WithFields(logrus.Fields{
		"total":      r.skills.Len(),
		"categories": r.byCategory.Len(),
		"triggers":   r.byTrigger.Len(),
		"failed":     len(failures),
	}).Info("Skills loaded successfully")
	if len(failures) > 0 {
		r.log.WithField("failed_count", len(failures)).
			Warn("Some skill files failed to parse during load — see LoadFailures() for the per-file report")
	}

	return nil
}

// LoadFromPath loads skills from a specific path.
func (r *Registry) LoadFromPath(ctx context.Context, path string) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	skills, failures, err := r.parser.ParseDirectory(path)
	if err != nil {
		return fmt.Errorf("failed to parse skills from path %s: %w", path, err)
	}
	r.recordLoadFailuresLocked(failures)

	for _, skill := range skills {
		r.registerSkillLocked(skill)
	}

	r.updateStatsLocked()
	return nil
}

// RegisterSkill registers a single skill.
func (r *Registry) RegisterSkill(skill *Skill) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.registerSkillLocked(skill)
	r.updateStatsLocked()
}

// registerSkillLocked internal registration (caller must hold writeMu).
func (r *Registry) registerSkillLocked(skill *Skill) {
	if skill.Name == "" {
		return
	}

	// Register by name
	r.skills.Put(skill.Name, skill)

	// Register by category
	if skill.Category != "" {
		r.byCategory.Update(skill.Category, func(curr []*Skill, _ bool) ([]*Skill, bool) {
			return append(curr, skill), true
		})
	}

	// Register by triggers
	for _, trigger := range skill.TriggerPhrases {
		r.byTrigger.Update(trigger, func(curr []*Skill, _ bool) ([]*Skill, bool) {
			return append(curr, skill), true
		})
	}

	r.log.WithFields(logrus.Fields{
		"skill":    skill.Name,
		"category": skill.Category,
		"triggers": len(skill.TriggerPhrases),
	}).Debug("Skill registered")
}

// Get retrieves a skill by name.
func (r *Registry) Get(name string) (*Skill, bool) {
	return r.skills.Get(name)
}

// GetByCategory retrieves all skills in a category.
func (r *Registry) GetByCategory(category string) []*Skill {
	skills, ok := r.byCategory.Get(category)
	if !ok {
		return nil
	}
	result := make([]*Skill, len(skills))
	copy(result, skills)
	return result
}

// GetByTrigger retrieves skills that match a trigger phrase.
func (r *Registry) GetByTrigger(trigger string) []*Skill {
	skills, ok := r.byTrigger.Get(trigger)
	if !ok {
		return nil
	}
	result := make([]*Skill, len(skills))
	copy(result, skills)
	return result
}

// GetAll returns all registered skills.
func (r *Registry) GetAll() []*Skill {
	return r.skills.Values()
}

// GetCategories returns all category names.
func (r *Registry) GetCategories() []string {
	return r.byCategory.Keys()
}

// GetTriggers returns all trigger phrases.
func (r *Registry) GetTriggers() []string {
	return r.byTrigger.Keys()
}

// Remove removes a skill from the registry.
func (r *Registry) Remove(name string) bool {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	skill, ok := r.skills.Get(name)
	if !ok {
		return false
	}

	// Remove from main map
	r.skills.Delete(name)

	// Remove from category index
	if skill.Category != "" {
		r.byCategory.Update(skill.Category, func(skills []*Skill, present bool) ([]*Skill, bool) {
			if !present {
				return nil, false
			}
			for i, s := range skills {
				if s.Name == name {
					skills = append(skills[:i], skills[i+1:]...)
					break
				}
			}
			return skills, len(skills) > 0
		})
	}

	// Remove from trigger index
	for _, trigger := range skill.TriggerPhrases {
		r.byTrigger.Update(trigger, func(skills []*Skill, present bool) ([]*Skill, bool) {
			if !present {
				return nil, false
			}
			for i, s := range skills {
				if s.Name == name {
					skills = append(skills[:i], skills[i+1:]...)
					break
				}
			}
			return skills, len(skills) > 0
		})
	}

	r.updateStatsLocked()
	return true
}

// Stats returns registry statistics.
func (r *Registry) Stats() *RegistryStats {
	current := r.stats.Load()
	if current == nil {
		return &RegistryStats{SkillsByCategory: make(map[string]int)}
	}
	stats := &RegistryStats{
		TotalSkills:      current.TotalSkills,
		SkillsByCategory: make(map[string]int, len(current.SkillsByCategory)),
		TotalTriggers:    current.TotalTriggers,
		LoadedAt:         current.LoadedAt,
		LastUpdated:      current.LastUpdated,
		FailedSkills:     current.FailedSkills,
	}
	for k, v := range current.SkillsByCategory {
		stats.SkillsByCategory[k] = v
	}
	return stats
}

// LoadFailures returns the per-file parse-failure report from the most
// recent Load/LoadFromPath (HXC-159 T-P6.04). Empty (never nil) when no
// failures occurred or no load has run yet — callers distinguish
// "zero failures" from "not yet loaded" via Stats().LoadedAt.IsZero().
func (r *Registry) LoadFailures() []ParseFailure {
	p := r.lastLoadFailures.Load()
	if p == nil {
		return []ParseFailure{}
	}
	out := make([]ParseFailure, len(*p))
	copy(out, *p)
	return out
}

// recordLoadFailuresLocked stores the parse-failure report for the load
// currently in progress. Caller must hold writeMu (both call sites do).
func (r *Registry) recordLoadFailuresLocked(failures []ParseFailure) {
	stored := make([]ParseFailure, len(failures))
	copy(stored, failures)
	r.lastLoadFailures.Store(&stored)
}

// updateStatsLocked recalculates registry statistics. Caller must hold writeMu.
func (r *Registry) updateStatsLocked() {
	prev := r.stats.Load()
	loadedAt := time.Time{}
	if prev != nil {
		loadedAt = prev.LoadedAt
	}
	if loadedAt.IsZero() {
		loadedAt = time.Now()
	}
	failedCount := 0
	if fp := r.lastLoadFailures.Load(); fp != nil {
		failedCount = len(*fp)
	}
	next := &RegistryStats{
		TotalSkills:      r.skills.Len(),
		TotalTriggers:    r.byTrigger.Len(),
		LoadedAt:         loadedAt,
		LastUpdated:      time.Now(),
		SkillsByCategory: make(map[string]int, r.byCategory.Len()),
		FailedSkills:     failedCount,
	}
	r.byCategory.Range(func(cat string, skills []*Skill) bool {
		next.SkillsByCategory[cat] = len(skills)
		return true
	})
	r.stats.Store(next)
}

// EnableHotReload enables automatic reloading of skills on file changes.
func (r *Registry) EnableHotReload(ctx context.Context) error {
	if !r.config.HotReload {
		return nil
	}

	r.watcher = &DirectoryWatcher{
		dir:      r.config.SkillsDirectory,
		interval: r.config.HotReloadInterval,
		stop:     make(chan struct{}),
		callback: func() {
			if err := r.Load(ctx); err != nil {
				r.log.WithError(err).Error("Failed to reload skills")
			}
		},
	}

	go r.watcher.start()
	r.log.Info("Hot reload enabled for skills")
	return nil
}

// DisableHotReload stops the hot reload watcher.
func (r *Registry) DisableHotReload() {
	if r.watcher != nil {
		close(r.watcher.stop)
		r.watcher = nil
	}
}

// start begins watching for file changes.
func (w *DirectoryWatcher) start() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			// Simple implementation: just reload periodically
			// A more sophisticated version would use fsnotify
			w.callback()
		}
	}
}

// Search searches for skills matching a query.
func (r *Registry) Search(query string) []*Skill {
	query = normalizeQuery(query)
	matches := make([]*Skill, 0)
	seen := make(map[string]bool)

	// Search by trigger
	r.byTrigger.Range(func(trigger string, skills []*Skill) bool {
		if containsIgnoreCase(trigger, query) || containsIgnoreCase(query, trigger) {
			for _, skill := range skills {
				if !seen[skill.Name] {
					matches = append(matches, skill)
					seen[skill.Name] = true
				}
			}
		}
		return true
	})

	// Search by name
	r.skills.Range(func(name string, skill *Skill) bool {
		if !seen[name] && containsIgnoreCase(name, query) {
			matches = append(matches, skill)
			seen[name] = true
		}
		return true
	})

	// Search by description
	r.skills.Range(func(_ string, skill *Skill) bool {
		if !seen[skill.Name] && containsIgnoreCase(skill.Description, query) {
			matches = append(matches, skill)
			seen[skill.Name] = true
		}
		return true
	})

	return matches
}
