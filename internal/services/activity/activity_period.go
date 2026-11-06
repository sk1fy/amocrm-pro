package activity

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// Stage 1 of period-v1: every day is computed on the fly from raw CRM events
// with the rating's bounded page scan. Nothing is cached, so no day is pending.

// ViewActivityPeriod returns week/month statistics of a panel's employees.
func (s *Service) ViewActivityPeriod(ctx context.Context, req serviceapi.ActivityPeriodRequest) (serviceapi.ActivityPeriod, error) {
	ctx, cancel := context.WithTimeout(ctx, serviceapi.ActivityPeriodTimeout)
	defer cancel()
	if err := serviceapi.ValidateActivityPeriodRequest(req); err != nil {
		return serviceapi.ActivityPeriod{}, err
	}
	_, panel, users, err := s.viewerContext(ctx, req.Auth, req.EmployeeIDs)
	if err != nil {
		return serviceapi.ActivityPeriod{}, err
	}
	loc, err := time.LoadLocation(panel.Timezone)
	if err != nil {
		return serviceapi.ActivityPeriod{}, serviceapi.Fail(serviceapi.Unavailable, "account timezone unavailable")
	}
	ids := req.EmployeeIDs
	if len(ids) == 0 {
		ids = panel.EmployeeIDs
	}
	clock := s.periodClock(loc, req.CutoffUnix)
	anchor, _ := time.Parse(time.DateOnly, req.Anchor)
	current := periodDays(req.Kind, anchor)
	previous := periodDays(req.Kind, previousPeriodStart(req.Kind, current[0]))
	w := periodWindow{Start: req.StartMinutes, End: req.EndMinutes}

	plan := make([]*periodScanDay, 0, len(current)+len(previous))
	for _, day := range current {
		plan = append(plan, clock.scanDay(day, w))
	}
	// The previous period is compared only when all its days exist; skip the
	// scan when the retention bound already rules that out.
	if !previous[0].Before(clock.historyCap) && previous[len(previous)-1].Before(clock.today) {
		for _, day := range previous {
			plan = append(plan, clock.scanDay(day, w))
		}
	}
	status, err := s.scanPeriod(ctx, req.Auth, panel, users, ids, plan)
	if err != nil {
		return serviceapi.ActivityPeriod{}, err
	}
	clock.applyStatus(status)
	scanned := map[string]*periodScanDay{}
	for _, d := range plan {
		scanned[dayKey(d.day)] = d
	}

	out := serviceapi.ActivityPeriod{
		FormulaVersion: serviceapi.ActivityPeriodFormula, Kind: req.Kind, Timezone: panel.Timezone,
		StartMinutes: req.StartMinutes, EndMinutes: req.EndMinutes, CutoffUnix: clock.cutoff, SnapshotAt: s.clock().Unix(),
		HistoryFrom: clock.historyFrom.Format(time.DateOnly), Hours: w.hours(),
		Current:   serviceapi.ActivityPeriodRange{Start: dayKey(current[0]), End: dayKey(current[len(current)-1]), Days: dayKeys(current)},
		Previous:  serviceapi.ActivityPreviousPeriod{ActivityPeriodRange: serviceapi.ActivityPeriodRange{Start: dayKey(previous[0]), End: dayKey(previous[len(previous)-1]), Days: dayKeys(previous)}, Available: true},
		Employees: make([]serviceapi.EmployeeActivityPeriod, 0, len(ids)),
	}
	for _, day := range previous {
		if clock.state(day) != serviceapi.ActivityDayReady {
			out.Previous.Available = false
		}
	}
	// "Up to the same point" exists only while the current period holds today.
	for i, day := range current {
		if day.Equal(clock.today) && i < len(previous) {
			out.Previous.Cut = &serviceapi.ActivityPeriodCut{DayIndex: i, Minute: clock.minute}
		}
	}
	pipelines := panel.Options.PipelineIDs
	for _, id := range ids {
		row := serviceapi.EmployeeActivityPeriod{EmployeeID: id, Current: serviceapi.ActivityPeriodCurrent{Days: make([]serviceapi.ActivityDayStat, 0, len(current))}}
		for _, day := range current {
			state := clock.state(day)
			var cut *int
			if state == serviceapi.ActivityDayToday {
				cut = &clock.minute
			}
			stat, _ := periodDayStat(day, state, scanned[dayKey(day)], status, id, w, cut, pipelines, loc)
			row.Current.Days = append(row.Current.Days, stat)
		}
		row.Current.Rating = periodRating(row.Current.Days)
		if out.Previous.Available {
			prev := &serviceapi.ActivityPeriodPrevious{Days: make([]serviceapi.ActivityDayStat, 0, len(previous))}
			for i, day := range previous {
				scan := scanned[dayKey(day)]
				stat, facts := periodDayStat(day, serviceapi.ActivityDayReady, scan, status, id, w, nil, pipelines, loc)
				prev.Days = append(prev.Days, stat)
				if c := out.Previous.Cut; c != nil && c.DayIndex == i {
					cutDay := periodDayStatFrom(day, serviceapi.ActivityDayReady, scan, status, facts, w, &clock.minute, pipelines)
					prev.CutDay = &cutDay
				}
			}
			prev.RatingFull = periodRating(prev.Days)
			prev.Rating = prev.RatingFull
			if c := out.Previous.Cut; c != nil {
				prev.Rating = periodRating(append(append([]serviceapi.ActivityDayStat{}, prev.Days[:c.DayIndex]...), *prev.CutDay))
			}
			row.Previous = prev
		}
		out.Employees = append(out.Employees, row)
	}
	if err := serviceapi.ValidateResponseSize(out); err != nil {
		return serviceapi.ActivityPeriod{}, err
	}
	return out, nil
}

// ViewActivityPeriodDays returns per-segment kinds and zones of one employee.
func (s *Service) ViewActivityPeriodDays(ctx context.Context, req serviceapi.ActivityPeriodDaysRequest) (serviceapi.ActivityPeriodDays, error) {
	ctx, cancel := context.WithTimeout(ctx, serviceapi.ActivityPeriodTimeout)
	defer cancel()
	if err := serviceapi.ValidateActivityPeriodDaysRequest(req); err != nil {
		return serviceapi.ActivityPeriodDays{}, err
	}
	_, panel, users, err := s.viewerContext(ctx, req.Auth, []int64{req.EmployeeID})
	if err != nil {
		return serviceapi.ActivityPeriodDays{}, err
	}
	loc, err := time.LoadLocation(panel.Timezone)
	if err != nil {
		return serviceapi.ActivityPeriodDays{}, serviceapi.Fail(serviceapi.Unavailable, "account timezone unavailable")
	}
	clock := s.periodClock(loc, req.CutoffUnix)
	from, _ := time.Parse(time.DateOnly, req.FromDay)
	to, _ := time.Parse(time.DateOnly, req.ToDay)
	w := periodWindow{Start: req.StartMinutes, End: req.EndMinutes}
	var days []time.Time
	plan := []*periodScanDay{}
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		days = append(days, day)
		plan = append(plan, clock.scanDay(day, w))
	}
	status, err := s.scanPeriod(ctx, req.Auth, panel, users, []int64{req.EmployeeID}, plan)
	if err != nil {
		return serviceapi.ActivityPeriodDays{}, err
	}
	clock.applyStatus(status)
	out := serviceapi.ActivityPeriodDays{
		FormulaVersion: serviceapi.ActivityPeriodFormula, Timezone: panel.Timezone, EmployeeID: req.EmployeeID,
		StartMinutes: req.StartMinutes, EndMinutes: req.EndMinutes, SegmentMinutes: serviceapi.ActivityPeriodSegmentMinutes,
		CutoffUnix: clock.cutoff, SnapshotAt: s.clock().Unix(), HistoryFrom: clock.historyFrom.Format(time.DateOnly),
		Days: make([]serviceapi.ActivityPeriodDay, 0, len(days)),
	}
	for i, day := range days {
		row := serviceapi.ActivityPeriodDay{Day: dayKey(day), State: clock.state(day)}
		if row.State == serviceapi.ActivityDayReady || row.State == serviceapi.ActivityDayToday {
			var cut *int
			if row.State == serviceapi.ActivityDayToday {
				cut = &clock.minute
			}
			scan := plan[i]
			stat := DayStatFrom(BuildDayFacts(scan.events[req.EmployeeID], row.Day, loc), w, cut, panel.Options.PipelineIDs)
			kinds, zones := stat.segments()
			row.ActivityPeriodDaySegments = &serviceapi.ActivityPeriodDaySegments{Coverage: scan.coverage(status), Partial: scan.partial, First: stat.First, Last: stat.Last, Kinds: kinds, Zones: zones}
		}
		out.Days = append(out.Days, row)
	}
	if err := serviceapi.ValidateResponseSize(out); err != nil {
		return serviceapi.ActivityPeriodDays{}, err
	}
	return out, nil
}

// periodClock fixes "today", the cut minute and the history bound of one
// request. Days are civil dates (UTC midnight) so DST never shifts them.
type periodClock struct {
	loc         *time.Location
	cutoff      int64
	minute      int
	today       time.Time
	historyCap  time.Time
	historyFrom time.Time
}

func (s *Service) periodClock(loc *time.Location, cutoff *int64) *periodClock {
	now := s.clock()
	c := &periodClock{loc: loc, cutoff: now.Unix()}
	if cutoff != nil {
		c.cutoff = *cutoff
	}
	at := time.Unix(c.cutoff, 0).In(loc)
	c.minute = at.Hour()*60 + at.Minute()
	c.today = civilDay(at)
	// Retention keeps 33 x 24 hours: today and the 32 whole days before it.
	c.historyCap = civilDay(now.In(loc)).AddDate(0, 0, 1-serviceapi.ActivityPeriodHistoryDays)
	c.historyFrom = c.historyCap
	return c
}

// applyStatus narrows history to the first whole day CRM Events still holds.
func (c *periodClock) applyStatus(status serviceapi.SyncStatus) {
	if status.HistoryFrom <= 0 {
		return
	}
	first := civilDay(time.Unix(status.HistoryFrom, 0).In(c.loc))
	if localStart(first, c.loc).Unix() < status.HistoryFrom {
		first = first.AddDate(0, 0, 1)
	}
	if first.After(c.historyFrom) {
		c.historyFrom = first
	}
}

func (c *periodClock) state(day time.Time) string {
	switch {
	case day.After(c.today):
		return serviceapi.ActivityDayFuture
	case day.Before(c.historyFrom):
		return serviceapi.ActivityDayUnavailable
	case day.Equal(c.today):
		return serviceapi.ActivityDayToday
	}
	return serviceapi.ActivityDayReady
}

// scanDay plans the event read of one day: from local midnight to the window
// end (plus an hour for the dashboard cut rule and DST), and never past the
// cutoff. Query bounds are inclusive. Days outside history are not read.
func (c *periodClock) scanDay(day time.Time, w periodWindow) *periodScanDay {
	d := &periodScanDay{day: day, events: map[int64][]periodEvent{}}
	if day.After(c.today) || day.Before(c.historyCap) {
		return d
	}
	start := localStart(day, c.loc)
	d.from = start.Unix()
	d.to = localStart(day.AddDate(0, 0, 1), c.loc).Unix() - 1
	if w.End < 1440 {
		d.to = min(d.to, time.Date(day.Year(), day.Month(), day.Day(), 0, w.End+60, 0, 0, c.loc).Unix()-1)
	}
	if day.Equal(c.today) {
		d.to = min(d.to, c.cutoff)
	}
	d.read = d.to > d.from
	return d
}

type periodScanDay struct {
	day      time.Time
	from, to int64
	read     bool
	events   map[int64][]periodEvent
	partial  bool
	status   *serviceapi.SyncStatus
	// Calls read without call facts or a payload duration, in scan order.
	cards []periodCard
}

type periodCard struct {
	id       string
	employee int64
	index    int
}

func (d *periodScanDay) coverage(status serviceapi.SyncStatus) string {
	to := max(d.to, d.from+1)
	coverage, _, _ := periodState(serviceapi.Query{From: d.from, To: to}, serviceapi.QueryResult{Status: status})
	return coverage
}

// scanPeriod reads the planned days with at most ActivityPeriodScanWorkers in
// parallel. A day that hits the page bound or the scan budget is partial; the
// returned status is the first planned day's.
func (s *Service) scanPeriod(ctx context.Context, auth serviceapi.Auth, panel serviceapi.ManagedPanel, users []serviceapi.User, ids []int64, plan []*periodScanDay) (serviceapi.SyncStatus, error) {
	budget := s.periodScanBudget
	if budget <= 0 {
		budget = serviceapi.ActivityPeriodScanBudget
	}
	deadline := time.Now().Add(budget)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan *periodScanDay)
	var failure error
	var once sync.Once
	var workers sync.WaitGroup
	for range serviceapi.ActivityPeriodScanWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for d := range work {
				if err := s.scanPeriodDay(ctx, auth, panel, users, ids, deadline, d); err != nil {
					once.Do(func() { failure = err; cancel() })
				}
			}
		}()
	}
feed:
	for _, d := range plan {
		if !d.read {
			continue
		}
		select {
		case work <- d:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	workers.Wait()
	if failure != nil {
		return serviceapi.SyncStatus{}, failure
	}
	if err := ctx.Err(); err != nil {
		return serviceapi.SyncStatus{}, err
	}
	if err := s.resolvePeriodCards(ctx, auth, plan, deadline); err != nil {
		return serviceapi.SyncStatus{}, err
	}
	for _, d := range plan {
		if d.status != nil {
			return *d.status, nil
		}
	}
	// Nothing was read (a future or expired period, or an exhausted budget):
	// one bounded probe still tells the history bound.
	now := s.clock().Unix()
	data, err := s.viewerQuery(ctx, serviceapi.Query{Auth: auth, From: now - 60, To: now, UserIDs: ids, Limit: 1}, panel, users, false)
	if err != nil {
		return serviceapi.SyncStatus{}, err
	}
	return data.Status, nil
}

func (s *Service) scanPeriodDay(ctx context.Context, auth serviceapi.Auth, panel serviceapi.ManagedPanel, users []serviceapi.User, ids []int64, deadline time.Time, d *periodScanDay) error {
	q := serviceapi.Query{Auth: auth, From: d.from, To: d.to, UserIDs: ids, Limit: 100}
	seen := map[string]bool{}
	for page := 0; page < serviceapi.ActivityPeriodDayMaxPages; page++ {
		if time.Now().After(deadline) {
			d.partial = true
			return nil
		}
		data, err := s.viewerQuery(ctx, q, panel, users, false)
		if err != nil {
			return err
		}
		if d.status == nil {
			status := data.Status
			d.status = &status
		}
		for _, event := range data.Events {
			e := periodEventFromAPI(event)
			list := d.events[event.CreatedBy]
			if event.CallFacts == nil && e.Duration == nil && (event.Type == "incoming_call" || event.Type == "outgoing_call") {
				d.cards = append(d.cards, periodCard{id: event.ID, employee: event.CreatedBy, index: len(list)})
			}
			d.events[event.CreatedBy] = append(list, e)
		}
		if data.NextCursor == "" {
			return nil
		}
		if seen[data.NextCursor] {
			return serviceapi.Fail(serviceapi.Unavailable, "repeated activity period event cursor")
		}
		seen[data.NextCursor] = true
		q.Cursor = data.NextCursor
	}
	d.partial = true
	return nil
}

// resolvePeriodCards repeats the rating's card fallback for calls without a
// known duration: at most 150 cards per request with four workers, newest
// days first (today matches the daily screen), within the scan budget. Cards
// left unresolved when the budget ends make their day partial.
func (s *Service) resolvePeriodCards(ctx context.Context, auth serviceapi.Auth, plan []*periodScanDay, deadline time.Time) error {
	reader, ok := s.events.(serviceapi.EventReader)
	if !ok {
		return nil
	}
	type ref struct {
		day  *periodScanDay
		card periodCard
	}
	days := slices.Clone(plan)
	slices.SortStableFunc(days, func(a, b *periodScanDay) int { return b.day.Compare(a.day) })
	var refs []ref
	var candidates []ratingCardCandidate
	for _, d := range days {
		for _, card := range d.cards {
			if len(refs) == serviceapi.ActivityPeriodMaxCards {
				break
			}
			candidates = append(candidates, ratingCardCandidate{ID: card.id, Index: len(refs)})
			refs = append(refs, ref{day: d, card: card})
		}
	}
	if len(refs) == 0 {
		return nil
	}
	observations := make([]ratingObservation, len(refs))
	cardCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	err := resolveRatingCards(cardCtx, reader, auth, observations, candidates)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for i, r := range refs {
		if duration := observations[i].Event.Duration; duration != nil {
			r.day.events[r.card.employee][r.card.index].Duration = duration
		} else if err != nil {
			r.day.partial = true
		}
	}
	return nil
}

func periodDayStat(day time.Time, state string, scan *periodScanDay, status serviceapi.SyncStatus, id int64, w periodWindow, cut *int, pipelines []int64, loc *time.Location) (serviceapi.ActivityDayStat, DayFacts) {
	if state != serviceapi.ActivityDayReady && state != serviceapi.ActivityDayToday {
		return serviceapi.ActivityDayStat{Day: dayKey(day), State: state}, DayFacts{}
	}
	facts := BuildDayFacts(scan.events[id], dayKey(day), loc)
	return periodDayStatFrom(day, state, scan, status, facts, w, cut, pipelines), facts
}

func periodDayStatFrom(day time.Time, state string, scan *periodScanDay, status serviceapi.SyncStatus, facts DayFacts, w periodWindow, cut *int, pipelines []int64) serviceapi.ActivityDayStat {
	stat := DayStatFrom(facts, w, cut, pipelines)
	return serviceapi.ActivityDayStat{
		Day: dayKey(day), State: state, Counted: stat.counted(day),
		ActivityDayValues: &serviceapi.ActivityDayValues{
			Coverage: scan.coverage(status), Partial: scan.partial,
			KPI: append([]int(nil), stat.KPI[:]...), Events: stat.Events,
			Active: stat.Active, Call: stat.Call, Elapsed: stat.Elapsed, IdleInside: stat.IdleInside,
			First: stat.First, Last: stat.Last, Hours: stat.Hours,
		},
	}
}

// periodRating is R1: Σactive / Σelapsed over counted days, null without time.
func periodRating(days []serviceapi.ActivityDayStat) *float64 {
	active, elapsed := 0, 0
	for _, d := range days {
		if d.Counted && d.ActivityDayValues != nil {
			active += d.Active
			elapsed += d.Elapsed
		}
	}
	if elapsed == 0 {
		return nil
	}
	rating := float64(active) / float64(elapsed)
	return &rating
}

// periodDays lists a Monday–Sunday week or a calendar month holding anchor.
func periodDays(kind string, anchor time.Time) []time.Time {
	start, end := anchor.AddDate(0, 0, -((int(anchor.Weekday())+6)%7)), time.Time{}
	if kind == serviceapi.ActivityPeriodMonth {
		start = time.Date(anchor.Year(), anchor.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	} else {
		end = start.AddDate(0, 0, 7)
	}
	days := []time.Time{}
	for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
		days = append(days, day)
	}
	return days
}

func previousPeriodStart(kind string, start time.Time) time.Time {
	if kind == serviceapi.ActivityPeriodMonth {
		return start.AddDate(0, -1, 0)
	}
	return start.AddDate(0, 0, -7)
}

func civilDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func localStart(day time.Time, loc *time.Location) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
}

func dayKey(day time.Time) string { return day.Format(time.DateOnly) }

func dayKeys(days []time.Time) []string {
	out := make([]string, 0, len(days))
	for _, day := range days {
		out = append(out, dayKey(day))
	}
	return out
}
