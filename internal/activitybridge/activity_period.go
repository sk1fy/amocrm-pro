package activitybridge

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func (b *Bridge) ViewActivityPeriodHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := b.issueViewer(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	req, err := decodeActivityPeriod(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	req.Auth = auth
	reader, ctx, cancel, ok := b.periodReader(w, r)
	if !ok {
		return
	}
	defer cancel()
	out, err := reader.ViewActivityPeriod(ctx, req)
	respond(w, http.StatusOK, out, shareErr(err))
}

func (b *Bridge) ViewActivityPeriodDaysHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := b.issueViewer(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	id, err := pathInt64(r, "employeeId")
	if err != nil {
		writeShareError(w, err)
		return
	}
	req, err := decodeActivityPeriodDays(r, id)
	if err != nil {
		writeShareError(w, err)
		return
	}
	req.Auth = auth
	reader, ctx, cancel, ok := b.periodReader(w, r)
	if !ok {
		return
	}
	defer cancel()
	out, err := reader.ViewActivityPeriodDays(ctx, req)
	respond(w, http.StatusOK, out, shareErr(err))
}

// periodReader extends only this response's write deadline, as the rating
// route does, so the finite period deadline can complete.
func (b *Bridge) periodReader(w http.ResponseWriter, r *http.Request) (serviceapi.ActivityPeriodReader, context.Context, context.CancelFunc, bool) {
	reader, ok := b.activity.(serviceapi.ActivityPeriodReader)
	if !ok {
		writeShareError(w, serviceapi.Fail(serviceapi.Unavailable, "Activity period service unavailable"))
		return nil, nil, nil, false
	}
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(serviceapi.ActivityPeriodTimeout + time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeShareError(w, serviceapi.Fail(serviceapi.Unavailable, "Activity period response deadline unavailable"))
		return nil, nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), serviceapi.ActivityPeriodTimeout)
	return reader, ctx, cancel, true
}

// periodQuery accepts each allowed parameter at most once and requires the
// listed ones.
func periodQuery(r *http.Request, allowed map[string]bool, required ...string) (url.Values, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, serviceapi.Fail(serviceapi.InvalidArgument, "malformed activity period query")
	}
	for k, v := range values {
		if !allowed[k] || len(v) != 1 {
			return nil, serviceapi.Fail(serviceapi.InvalidArgument, "unknown or repeated activity period parameter")
		}
	}
	for _, k := range required {
		if _, ok := values[k]; !ok {
			return nil, serviceapi.Fail(serviceapi.InvalidArgument, "missing "+k)
		}
	}
	return values, nil
}

func periodWindow(values url.Values) (start, end int, cutoff *int64, err error) {
	if start, err = strconv.Atoi(values.Get("start_minutes")); err != nil {
		return 0, 0, nil, serviceapi.Fail(serviceapi.InvalidArgument, "invalid start_minutes")
	}
	if end, err = strconv.Atoi(values.Get("end_minutes")); err != nil {
		return 0, 0, nil, serviceapi.Fail(serviceapi.InvalidArgument, "invalid end_minutes")
	}
	if value, ok := values["cutoff_unix"]; ok {
		cut, err := strconv.ParseInt(value[0], 10, 64)
		if err != nil {
			return 0, 0, nil, serviceapi.Fail(serviceapi.InvalidArgument, "invalid cutoff_unix")
		}
		cutoff = &cut
	}
	return start, end, cutoff, nil
}

func decodeActivityPeriod(r *http.Request) (serviceapi.ActivityPeriodRequest, error) {
	var out serviceapi.ActivityPeriodRequest
	values, err := periodQuery(r, map[string]bool{"kind": true, "anchor": true, "start_minutes": true, "end_minutes": true, "cutoff_unix": true, "employee_ids": true}, "kind", "anchor", "start_minutes", "end_minutes")
	if err != nil {
		return out, err
	}
	out.Kind = values.Get("kind")
	out.Anchor = values.Get("anchor")
	if out.StartMinutes, out.EndMinutes, out.CutoffUnix, err = periodWindow(values); err != nil {
		return out, err
	}
	if value, ok := values["employee_ids"]; ok {
		for _, v := range strings.Split(value[0], ",") {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return out, serviceapi.Fail(serviceapi.InvalidArgument, "invalid employee_ids")
			}
			out.EmployeeIDs = append(out.EmployeeIDs, id)
		}
	}
	return out, serviceapi.ValidateActivityPeriodRequest(out)
}

func decodeActivityPeriodDays(r *http.Request, employeeID int64) (serviceapi.ActivityPeriodDaysRequest, error) {
	out := serviceapi.ActivityPeriodDaysRequest{EmployeeID: employeeID}
	values, err := periodQuery(r, map[string]bool{"from_day": true, "to_day": true, "start_minutes": true, "end_minutes": true, "cutoff_unix": true}, "from_day", "to_day", "start_minutes", "end_minutes")
	if err != nil {
		return out, err
	}
	out.FromDay = values.Get("from_day")
	out.ToDay = values.Get("to_day")
	if out.StartMinutes, out.EndMinutes, out.CutoffUnix, err = periodWindow(values); err != nil {
		return out, err
	}
	return out, serviceapi.ValidateActivityPeriodDaysRequest(out)
}
