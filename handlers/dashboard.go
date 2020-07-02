// Copyright © 2019 Martin Tournoij <martin@arp242.net>
// This file is part of GoatCounter and published under the terms of the EUPL
// v1.2, which can be found in the LICENSE file or at http://eupl12.zgo.at

package handlers

import (
	"context"
	"html/template"
	"net/http"
	"sort"
	"sync"
	"time"

	"zgo.at/errors"
	"zgo.at/goatcounter"
	"zgo.at/goatcounter/cfg"
	"zgo.at/zhttp"
	"zgo.at/zlog"
	"zgo.at/zstd/zstring"
	"zgo.at/zstd/zsync"
)

const day = 24 * time.Hour

type widget struct {
	Name string
	Type string // "full-width", "hchart"
	HTML template.HTML
}

type dashboardData struct {
	total, totalUnique int

	pages struct {
		display, uniqueDisplay int
		max                    int
		more                   bool
		pages                  goatcounter.HitStats
		refs                   goatcounter.Stats
	}

	totalPages struct {
		max   int
		total goatcounter.HitStat
	}

	topRefs  goatcounter.Stats
	browsers goatcounter.Stats
	systems  goatcounter.Stats
	sizeStat goatcounter.Stats
	locStat  goatcounter.Stats
}

func (h backend) dashboard(w http.ResponseWriter, r *http.Request) error {
	site := goatcounter.MustGetSite(r.Context())

	// Cache much more aggressively for public displays. Don't care so much if
	// it's outdated by an hour.
	if site.Settings.Public && goatcounter.GetUser(r.Context()).ID == 0 {
		w.Header().Set("Cache-Control", "public,max-age=3600")
		w.Header().Set("Vary", "Cookie")
	}

	start, end, err := getPeriod(w, r, site)
	if err != nil {
		zhttp.FlashError(w, err.Error())
	}
	if start.IsZero() || end.IsZero() {
		y, m, d := goatcounter.Now().In(site.Settings.Timezone.Loc()).Date()
		now := time.Date(y, m, d, 0, 0, 0, 0, site.Settings.Timezone.Loc())
		start = now.Add(-7 * day).UTC()
		end = time.Date(y, m, d, 23, 59, 59, 9, now.Location()).UTC().Round(time.Second)
	}

	showRefs := r.URL.Query().Get("showrefs")
	filter := r.URL.Query().Get("filter")
	daily, forcedDaily := getDaily(r, start, end)

	wantWidgets := []string{"totals",
		"pages", "totalpages", "toprefs", "browsers", "systems", "sizes", "locations"}
	if zstring.Contains(wantWidgets, "pages") {
		wantWidgets = append(wantWidgets, "max")
		if showRefs != "" {
			wantWidgets = append(wantWidgets, "refs")
		}
	}

	var (
		wg    sync.WaitGroup
		bgErr = errors.NewGroup(20)
		data  dashboardData
	)
	widgetData := map[string]func() error{
		"totals": func() (err error) {
			data.total, data.totalUnique, err = goatcounter.GetTotalCount(r.Context(), start, end, filter)
			return err
		},
		"pages": func() (err error) {
			data.pages.display, data.pages.uniqueDisplay, data.pages.more, err = data.pages.pages.List(
				r.Context(), start, end, filter, nil, daily)
			return err
		},
		"max": func() (err error) {
			data.pages.max, err = goatcounter.GetMax(r.Context(), start, end, filter, daily)
			return err
		},
		"totalpages": func() (err error) {
			data.totalPages.max, err = data.totalPages.total.Totals(r.Context(), start, end, filter, daily)
			return err
		},
		"refs":      func() (err error) { return data.pages.refs.ListRefsByPath(r.Context(), showRefs, start, end, 0) },
		"toprefs":   func() (err error) { return data.topRefs.ListTopRefs(r.Context(), start, end, 0) },
		"browsers":  func() (err error) { return data.browsers.ListBrowsers(r.Context(), start, end, 6, 0) },
		"systems":   func() (err error) { return data.systems.ListSystems(r.Context(), start, end, 6, 0) },
		"sizes":     func() (err error) { return data.sizeStat.ListSizes(r.Context(), start, end) },
		"locations": func() (err error) { return data.locStat.ListLocations(r.Context(), start, end, 6, 0) },
	}

	for _, w := range wantWidgets {
		wg.Add(1)
		go func(w string) {
			defer zlog.Recover(func(l zlog.Log) zlog.Log { return l.Field("data widget", w).FieldsRequest(r) })
			defer wg.Done()

			l := zlog.Module("dashboard")
			bgErr.Append(widgetData[w]())
			l.Since(w)
		}(w)
	}

	subs, err := site.ListSubs(r.Context())
	if err != nil {
		return err
	}

	cd := cfg.DomainCount
	if cd == "" {
		cd = goatcounter.MustGetSite(r.Context()).Domain()
		if cfg.Port != "" {
			cd += ":" + cfg.Port
		}
	}

	zsync.Wait(r.Context(), &wg)
	if bgErr.Len() > 0 {
		return bgErr
	}

	render := map[string]func() (string, string, interface{}){
		"pages": func() (string, string, interface{}) {
			return "full-width", "_dashboard_pages.gohtml", struct {
				Context     context.Context
				Pages       goatcounter.HitStats
				Site        *goatcounter.Site
				PeriodStart time.Time
				PeriodEnd   time.Time
				Daily       bool
				ForcedDaily bool
				Max         int

				TotalDisplay       int
				TotalUniqueDisplay int

				TotalHits       int
				TotalUniqueHits int
				MorePages       bool

				Refs         goatcounter.Stats
				ShowRefs     string
				IsPagination bool
			}{r.Context(), data.pages.pages, site, start, end, daily, forcedDaily, data.pages.max,
				data.pages.display, data.pages.uniqueDisplay,
				data.total, data.totalUnique, data.pages.more,
				data.pages.refs, showRefs, false}
		},
		"totalpages": func() (string, string, interface{}) {
			return "full-width", "_dashboard_totals.gohtml", struct {
				Context         context.Context
				Site            *goatcounter.Site
				Page            goatcounter.HitStat
				Daily           bool
				Max             int
				TotalHits       int
				TotalUniqueHits int
			}{r.Context(), site, data.totalPages.total, daily, data.totalPages.max,
				data.total, data.totalUnique}
		},
		"toprefs": func() (string, string, interface{}) {
			return "hchart", "_dashboard_toprefs.gohtml", struct {
				Context         context.Context
				TotalUniqueHits int
				Stats           goatcounter.Stats
			}{r.Context(), data.totalUnique, data.topRefs}
		},
		"browsers": func() (string, string, interface{}) {
			return "hchart", "_dashboard_browsers.gohtml", struct {
				Context         context.Context
				TotalUniqueHits int
				Stats           goatcounter.Stats
			}{r.Context(), data.totalUnique, data.browsers}
		},
		"systems": func() (string, string, interface{}) {
			return "hchart", "_dashboard_systems.gohtml", struct {
				Context         context.Context
				TotalUniqueHits int
				Stats           goatcounter.Stats
			}{r.Context(), data.totalUnique, data.systems}
		},
		"sizes": func() (string, string, interface{}) {
			return "hchart", "_dashboard_sizes.gohtml", struct {
				Context         context.Context
				TotalUniqueHits int
				Stats           goatcounter.Stats
			}{r.Context(), data.totalUnique, data.sizeStat}
		},
		"locations": func() (string, string, interface{}) {
			return "hchart", "_dashboard_locations.gohtml", struct {
				Context         context.Context
				TotalUniqueHits int
				Stats           goatcounter.Stats
			}{r.Context(), data.totalUnique, data.locStat}
		},
	}

	var widgets []widget
	for _, w := range wantWidgets {
		wg.Add(1)
		go func(w string) {
			defer zlog.Recover(func(l zlog.Log) zlog.Log { return l.Field("tpl widget", w).FieldsRequest(r) })
			defer wg.Done()

			f, ok := render[w]
			if !ok {
				return
			}

			typ, tplName, tplData := f()
			tpl, err := zhttp.ExecuteTpl(tplName, tplData)
			if err != nil {
				bgErr.Append(err)
				return
			}
			widgets = append(widgets, widget{
				Name: w,
				Type: typ,
				HTML: template.HTML(tpl),
			})
		}(w)
	}

	zsync.Wait(r.Context(), &wg)
	if bgErr.Len() > 0 {
		return bgErr
	}

	// Ensure correct order.
	sortmap := make(map[string]int, len(wantWidgets))
	for i, w := range wantWidgets {
		sortmap[w] = i
	}
	sort.Slice(widgets, func(i, j int) bool { return sortmap[widgets[i].Name] < sortmap[widgets[j].Name] })

	return zhttp.Template(w, "dashboard.gohtml", struct {
		Globals
		CountDomain    string
		SubSites       []string
		ShowRefs       string
		SelectedPeriod string
		PeriodStart    time.Time
		PeriodEnd      time.Time
		Filter         string
		Daily          bool
		ForcedDaily    bool
		Widgets        []widget
	}{newGlobals(w, r),
		cd, subs, showRefs, r.URL.Query().Get("hl-period"), start, end, filter,
		daily, forcedDaily, widgets,
	})
}
