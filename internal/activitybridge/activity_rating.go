package activitybridge

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func (b *Bridge) ViewActivityRatingsHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := b.issueViewer(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	req, err := decodeActivityRating(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	req.Auth = auth
	reader, ok := b.activity.(serviceapi.ActivityRatingReader)
	if !ok {
		writeShareError(w, serviceapi.Fail(serviceapi.Unavailable, "Activity rating service unavailable"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	out, err := reader.ViewActivityRatings(ctx, req)
	respond(w, http.StatusOK, out, shareErr(err))
}
func decodeActivityRating(r *http.Request) (serviceapi.ActivityRatingRequest, error) {
	var out serviceapi.ActivityRatingRequest
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return out, serviceapi.Fail(serviceapi.InvalidArgument, "malformed activity rating query")
	}
	allowed := map[string]bool{"from": true, "to": true, "day": true, "surface": true, "start_minutes": true, "end_minutes": true, "cutoff_unix": true, "employee_ids": true}
	for k, v := range values {
		if !allowed[k] || len(v) != 1 {
			return out, serviceapi.Fail(serviceapi.InvalidArgument, "unknown or repeated activity rating parameter")
		}
	}
	out.Day = values.Get("day")
	out.Surface = values.Get("surface")
	out.From, err = strconv.ParseInt(values.Get("from"), 10, 64)
	if err != nil {
		return out, serviceapi.Fail(serviceapi.InvalidArgument, "invalid from")
	}
	out.To, err = strconv.ParseInt(values.Get("to"), 10, 64)
	if err != nil {
		return out, serviceapi.Fail(serviceapi.InvalidArgument, "invalid to")
	}
	out.StartMinutes, err = strconv.Atoi(values.Get("start_minutes"))
	if err != nil {
		return out, serviceapi.Fail(serviceapi.InvalidArgument, "invalid start_minutes")
	}
	out.EndMinutes, err = strconv.Atoi(values.Get("end_minutes"))
	if err != nil {
		return out, serviceapi.Fail(serviceapi.InvalidArgument, "invalid end_minutes")
	}
	if value, ok := values["cutoff_unix"]; ok {
		cut, err := strconv.ParseInt(value[0], 10, 64)
		if err != nil {
			return out, serviceapi.Fail(serviceapi.InvalidArgument, "invalid cutoff_unix")
		}
		out.CutoffUnix = &cut
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
	return out, serviceapi.ValidateActivityRatingRequest(out)
}
