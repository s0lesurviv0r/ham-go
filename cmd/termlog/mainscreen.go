package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/dh1tw/goHamlib"
	"github.com/dustin/go-humanize"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/nsf/termbox-go"

	"github.com/tzneal/ham-go"
	"github.com/tzneal/ham-go/adif"
	"github.com/tzneal/ham-go/cabrillo"
	"github.com/tzneal/ham-go/callsigns"
	"github.com/tzneal/ham-go/cmd/termlog/input"
	"github.com/tzneal/ham-go/cmd/termlog/ui"
	"github.com/tzneal/ham-go/db"
	"github.com/tzneal/ham-go/dxcc"
	"github.com/tzneal/ham-go/logingest"
	"github.com/tzneal/ham-go/logsync"
	"github.com/tzneal/ham-go/rig"
	"github.com/tzneal/ham-go/solar"
	"github.com/tzneal/ham-go/spotting"
)

type mainScreen struct {
	controller      *ui.MainController
	qso             *ui.QSO
	qsoList         *ui.QSOList
	alog            *adif.Log
	bookmarks       *ham.Bookmarks
	repo            *git.Repository
	cfg             *Config
	wsjtxLog        *logingest.WSJTXServer
	fldigiLog       *logingest.FLDIGIServer
	js8log          *logingest.JS8Server
	rig             *rig.RigCache
	d               *db.Database
	editingQSO      bool // are we editing a QSO, or creating a new one?
	messages        *ui.Messages
	toBeLogged      chan logRequest
	shutdown        chan struct{}
	lookup          callsigns.Lookup
	loggingReplaced bool
	solar           *solar.Solar
}
type logRequest struct {
	record   adif.Record
	external bool
}

// used to accept log writes
func (m *mainScreen) Write(p []byte) (n int, err error) {
	m.logErrorf(string(p))
	return len(p), nil
}

func newMainScreen(cfg *Config, alog *adif.Log, repo *git.Repository, bookmarks *ham.Bookmarks, rig *rig.RigCache,
	d *db.Database) *mainScreen {
	c := ui.NewController(cfg.Theme)
	c.RefreshEvery(250 * time.Millisecond)

	_, remainingHeight := termbox.Size()

	// status bar
	yPos := 0
	sb := ui.NewStatusBar(yPos)
	sb.AddText("termlog " + ham.Version)
	sb.AddClock("Local")
	sb.AddText("/")
	sb.AddClock("UTC")
	if cfg.Operator.Sig != "" && cfg.Operator.SigInfo != "" {
		sb.AddText("/")
		sb.AddText(cfg.Operator.Sig)
		sb.AddText(cfg.Operator.SigInfo)
	}
	c.AddWidget(sb)
	yPos++
	remainingHeight--

	lookup := callsigns.BuildLookup(cfg.Lookup, cfg.noNet)
	qso := ui.NewQSO(yPos, c.Theme(), lookup, cfg.Operator.CustomFields, rig)
	c.AddWidget(qso)
	yPos += qso.Height()
	remainingHeight -= qso.Height()

	// default to a size
	qsoHeight := 12
	msgHeight := 5

	// but fill the screen if the spotting is disbled
	if !cfg.DXCluster.Enabled && !cfg.POTASpot.Enabled {
		// - 1 due to the status bar
		qsoHeight = remainingHeight - 1 - msgHeight
	}

	qsoList := ui.NewQSOList(yPos, alog, qsoHeight, cfg.Theme)
	qso.SetOperatorGrid(cfg.Operator.Grid)
	qsoList.SetOperatorGrid(cfg.Operator.Grid)
	c.AddWidget(qsoList)
	yPos += qsoHeight
	remainingHeight -= qsoHeight

	// is the spot monitoring enabled?
	shutdown := make(chan struct{})
	if !cfg.noNet && (cfg.DXCluster.Enabled || cfg.POTASpot.Enabled ||
		cfg.SOTASpot.Enabled) {
		// create the UI
		dxHeight := remainingHeight - 1 - msgHeight // -1 due to status bar
		spotlist := ui.NewSpottingList(yPos, dxHeight, time.Duration(cfg.Operator.SpotExpiration)*time.Second, cfg.Theme)
		if rig != nil {
			spotlist.OnTune(func(f float64) {
				f = f * 1e6
				// ensure we are in the proper mode
				if f < 10000000 {
					rig.SetMode(goHamlib.VFOCurrent, goHamlib.ModeLSB, 0)
				} else {
					rig.SetMode(goHamlib.VFOCurrent, goHamlib.ModeUSB, 0)
				}
				rig.SetFreq(goHamlib.VFOCurrent, f)
			})
		}
		c.AddWidget(spotlist)
		yPos += dxHeight

		if cfg.DXCluster.Enabled {
			dcfg := spotting.DXClusterConfig{
				Network:    "tcp",
				Address:    fmt.Sprintf("%s:%d", cfg.DXCluster.Server, cfg.DXCluster.Port),
				Callsign:   cfg.Operator.Call,
				ZoneLookup: cfg.DXCluster.ZoneLookup,
			}
			dxclient := spotting.NewDXClusterClient(dcfg)
			dxclient.Run()
			go func() {
				for {
					select {
					case <-shutdown:
						return
					case spot := <-dxclient.Spots:
						st := fmt.Sprintf("%s %s", time.Now().Format("02 Jan 06"), spot.Time)
						tm, err := time.Parse("02 Jan 06 1504Z", st)
						if err != nil {
							log.Printf("error parsing DX time: %s", err)
						}
						spotlist.AddSpot(ui.SpotRecord{
							Source:    "DX",
							Frequency: spot.Frequency,
							Station:   spot.DXStation,
							Comment:   spot.Comment,
							Time:      tm.Local(),
							Location:  spot.Location,
						})
					}
				}
			}()
		}
		if cfg.POTASpot.Enabled {
			pcfg := spotting.POTAConfig{
				URL: cfg.POTASpot.URL,
			}
			pclient := spotting.NewPOTAClient(pcfg)
			pclient.Run()
			go func() {
				for {
					select {
					case <-shutdown:
						return
					case spot := <-pclient.Spots:
						freq, _ := strconv.ParseFloat(spot.Frequency, 64)
						tm, _ := spot.Time()
						location := spot.Reference + "/" + spot.ParkName
						if spot.LocationDescription != "" {
							location = fmt.Sprintf("%s/%s/%s", spot.Reference, spot.ParkName, spot.LocationDescription)
						}
						spotlist.AddSpot(ui.SpotRecord{
							Source:    "POTA",
							Frequency: freq,
							Station:   spot.Activator,
							Comment:   spot.Comments,
							Time:      tm.Local(),
							Location:  location,
						})
						_ = spot
					}
				}
			}()
		}

		if cfg.SOTASpot.Enabled {
			pcfg := spotting.SOTAConfig{
				URL: cfg.SOTASpot.URL,
			}
			pclient := spotting.NewSOTAClient(pcfg)
			pclient.Run()
			go func() {
				for {
					select {
					case <-shutdown:
						return
					case spot := <-pclient.Spots:
						freq, _ := strconv.ParseFloat(spot.Frequency, 64)
						tm, _ := spot.Time()
						location := spot.SummitCode
						if spot.SummitDetails != "" {
							location = fmt.Sprintf("%s/%s", spot.SummitCode, spot.SummitDetails)
						}
						spotlist.AddSpot(ui.SpotRecord{
							Source:    "SOTA",
							Frequency: freq * 1e3,
							Station:   spot.ActivatorCallsign,
							Comment:   spot.Comments,
							Time:      tm.Local(),
							Location:  location,
						})
						_ = spot
					}
				}
			}()
		}
	}

	msgs := ui.NewMessages(yPos, msgHeight, cfg.Theme)
	c.AddWidget(msgs)

	fb := ui.NewStatusBar(-1)
	if rig != nil {
		fb.AddText(rig.Rig.Caps.MfgName)
		fb.AddText(rig.Rig.Caps.ModelName)
		fb.AddFunction(func() string {
			mode, _, err := rig.GetMode(goHamlib.VFOCurrent)
			if err == nil {
				return goHamlib.ModeName[mode]
			}
			return ""
		}, 5)
	}

	fb.AddFunction(func() string {
		freq, err := strconv.ParseFloat(qso.Frequency(), 64)
		if err != nil {
			return ""
		}
		sb := strings.Builder{}
		for _, lbl := range cfg.Label {
			if freq >= lbl.Start && freq <= lbl.End {
				if sb.Len() > 0 {
					sb.WriteByte('/')
				}
				sb.WriteString(lbl.Name)
			}
		}
		return sb.String()
	}, 20)

	fb.AddFunction(func() string {
		call := qso.Call()
		if call == "" {
			return ""
		}
		res, _ := d.Search(call)
		switch len(res) {
		case 0:
			return fmt.Sprintf("Have never seen %s", call)
		case 1:
			return fmt.Sprintf("Seen once at %s (%s)", adif.UTCTimestamp(res[0].Date), humanize.RelTime(res[0].Date, time.Now(), "ago", ""))
		default:
			last := res[len(res)-1].Date
			return fmt.Sprintf("Seen %d times, first %s last %s (%s)", len(res), adif.UTCTimestamp(res[0].Date), adif.UTCTimestamp(last), humanize.RelTime(last, time.Now(), "ago", ""))
		}
	}, 80)

	c.AddWidget(fb)

	c.Focus(qso)
	ms := &mainScreen{
		controller: c,
		qso:        qso,
		qsoList:    qsoList,
		alog:       alog,
		repo:       repo,
		cfg:        cfg,
		rig:        rig,
		messages:   msgs,
		bookmarks:  bookmarks,
		editingQSO: false,
		lookup:     lookup,
		d:          d,
		shutdown:   shutdown,
		toBeLogged: make(chan logRequest),
	}

	go ms.logRoutine()

	log.SetFlags(0)
	log.SetOutput(ms)
	goHamlib.SetDebugCallback(ms.handleHamlibDebug)
	ms.loggingReplaced = true

	qsoList.OnSelect(func(r adif.Record) {
		if !qso.HasRig() {
			qso.SetRecord(r)
			ms.editingQSO = true
		}
	})

	if cfg.WSJTX.Enabled {
		wsjtxLog, err := logingest.NewWSJTXServer(cfg.WSJTX.Address)
		if err != nil {
			ms.logErrorf("error launching WSJTx server: %s", err)
		} else {
			ms.logInfo("accepting logs from WSJT-X at %s", cfg.WSJTX.Address)
			ms.wsjtxLog = wsjtxLog
			ms.wsjtxLog.Run()
		}
	}

	if cfg.JS8Call.Enabled {
		js8log, err := logingest.NewJS8Server(cfg.JS8Call.Address)
		if err != nil {
			ms.logErrorf("error launching JS8Call server: %s", err)
		} else {
			ms.logInfo("accepting logs from JS8Call at %s", cfg.JS8Call.Address)
			ms.js8log = js8log
			ms.js8log.Run()
		}
	}

	if cfg.FLLog.Enabled {
		fldigiLog, err := logingest.NewFLDIGIServer(cfg.FLLog.Address)
		if err != nil {
			ms.logErrorf("error launching fldigi server: %s", err)
		} else {
			ms.logInfo("accepting logs from fldigi at %s", cfg.FLLog.Address)
			ms.fldigiLog = fldigiLog
			ms.fldigiLog.Run()
		}
	}

	if !cfg.noNet && cfg.HamQSL.Enabled {
		hcfg := solar.HamQSLConfig{
			URL: cfg.HamQSL.URL,
		}
		hclient := solar.NewHamQSLClient(hcfg)
		hclient.Run()
		go func() {
			for {
				select {
				case <-shutdown:
					return
				case s := <-hclient.Solar:
					ms.solar = &s
					_ = s
				}
			}
		}()
	}

	c.AddCommand(input.KeyCtrlH, ms.showHelp)
	c.AddCommand(input.KeyCtrlL, ms.focusQSOList)
	c.AddCommand(input.KeyCtrlN, ms.newQSO)
	c.AddCommand(input.KeyCtrlD, ms.qso.ResetDateTime)
	c.AddCommand(input.KeyCtrlS, ms.saveQSO)
	c.AddCommand(input.KeyAltB, ms.listBookmarks)
	c.AddCommand(input.KeyCtrlB, ms.saveBookmark)
	c.AddCommand(input.KeyCtrlG, ms.commitLog)
	c.AddCommand(input.KeyCtrlR, ms.redrawAll)
	c.AddCommand(input.KeyCtrlX, ms.exportCabrillo)
	c.AddCommand(input.KeyCtrlC, ms.showConditions)

	c.AddCommand(input.KeyCtrlE, ms.executeCommands)
	c.AddCommand(input.KeyAltLeft, ms.tuneLeft)
	c.AddCommand(input.KeyAltRight, ms.tuneRight)
	return ms
}

// logRoutine accepts QSOs, looks up additional informatin, logs to LoTW, etc. before logging them
// to a file
func (m *mainScreen) logRoutine() {
	for {
		select {
		case <-m.shutdown:
			return
		case rec := <-m.toBeLogged:

			// perform lookup on external submitted QSOs (e.g. from WSJT-X)
			if rec.external {
				rsp, err := m.lookup.Lookup(rec.record.Get(adif.Call))
				if err == nil {
					if rsp.Name != nil && rec.record.Get(adif.Name) == "" {
						rec.record = append(rec.record, adif.Field{Name: adif.Name, Value: *rsp.Name})
					}
					if rsp.Grid != nil && rec.record.Get(adif.GridSquare) == "" {
						rec.record = append(rec.record, adif.Field{Name: adif.GridSquare, Value: *rsp.Grid})
					}
					if rsp.Country != nil {
						rec.record = append(rec.record, adif.Field{Name: adif.Country, Value: *rsp.Country})

						ent, err := dxcc.LookupEntity(*rsp.Country)
						if err == nil {
							rec.record = append(rec.record,
								adif.Field{
									Name:  adif.DXCC,
									Value: strconv.FormatInt(int64(ent.DXCC), 10),
								})
						}
					}
				}
			}

			if m.cfg.Operator.Sig != "" {
				rec.record = append(rec.record, adif.Field{Name: adif.MySIG, Value: m.cfg.Operator.Sig})
			}

			if m.cfg.Operator.SigInfo != "" {
				rec.record = append(rec.record, adif.Field{Name: adif.MySIGInfo, Value: m.cfg.Operator.SigInfo})
			}

			if m.cfg.Operator.Call != "" {
				rec.record = append(
					rec.record, adif.Field{Name: adif.Call, Value: m.cfg.Operator.Call})
			}

			if m.cfg.Operator.Grid != "" {
				rec.record = append(
					rec.record, adif.Field{Name: adif.MyGridSquare, Value: m.cfg.Operator.Grid})
			}

			if m.cfg.Operator.City != "" {
				rec.record = append(
					rec.record, adif.Field{Name: adif.MyCity, Value: m.cfg.Operator.City})
			}

			if m.cfg.Operator.County != "" {
				rec.record = append(
					rec.record, adif.Field{Name: adif.MyCounty, Value: m.cfg.Operator.County})
			}

			if m.cfg.Operator.Country != "" {
				rec.record = append(
					rec.record, adif.Field{Name: adif.MyCountry, Value: m.cfg.Operator.Country})
			}

			if m.cfg.Operator.Antenna != "" {
				rec.record = append(
					rec.record, adif.Field{Name: adif.MyAntenna, Value: m.cfg.Operator.Antenna})
			}

			if m.cfg.Rig.Manufacturer != "" && m.cfg.Rig.Model != "" {
				rig := m.cfg.Rig.Manufacturer + " " + m.cfg.Rig.Model
				rec.record = append(
					rec.record, adif.Field{Name: adif.MyRig, Value: rig})
			}

			if rec.record.Get(adif.TXPower) != "" {
				rec.record = append(
					rec.record, adif.Field{Name: adif.TXPower, Value: rec.record.Get(adif.TXPower)})
			} else if m.cfg.Operator.Power > 0 {
				rec.record = append(
					rec.record, adif.Field{Name: adif.TXPower, Value: fmt.Sprintf("%d", m.cfg.Operator.Power)})
			}

			// upload to LoTW?
			if !m.cfg.noNet && m.cfg.Operator.LOTWAutoUpload {
				// possibly adds new fields if successful
				rec.record = m.logToLOTW(rec.record)
			}

			m.alog.AddRecord(rec.record)
			m.alog.Save()

			// index the QSO so we can quickly identify if we've seen it before
			r, err := db.AdifToRecord(rec.record)
			if err == nil {
				m.d.AddRecord(r)
			}
		}
	}
}

func (m *mainScreen) exportCabrillo() {
	exportFilename, ok := ui.InputString(m.controller, "Enter Export Filename")
	if !ok {
		return
	}
	cl := cabrillo.Log{}
	cl.Name = m.cfg.Operator.Name
	cl.Email = m.cfg.Operator.Email
	cl.Callsign = m.cfg.Operator.Call
	cl.CategoryAssisted, ok = ui.InputBool(m.controller, "Assisted")
	if !ok {
		return
	}

	cl.Contest, ok = ui.InputString(m.controller, "Contest")
	if !ok {
		return
	}
	co, ok := ui.InputChoice(m.controller, "Operator", []string{"SINGLE-OP", "MULTI-OP", "CHECKLOG"})
	switch co {
	case "SINGLE-OP":
		cl.CategoryOperator = cabrillo.CategoryOperatorSingle
	case "MULTI-OP":
		cl.CategoryOperator = cabrillo.CategoryOperatorMulti
	default:
		cl.CategoryOperator = cabrillo.CategoryOperatorChecklog
	}
	if !ok {
		return
	}

	co, ok = ui.InputChoice(m.controller, "Power", []string{"HIGH", "LOW", "QRP"})
	switch co {
	case "HIGH":
		cl.CategoryPower = cabrillo.CategoryPowerHigh
	case "LOW":
		cl.CategoryPower = cabrillo.CategoryPowerLow
	default:
		cl.CategoryPower = cabrillo.CategoryPowerQRP
	}
	if !ok {
		return
	}

	co, ok = ui.InputChoice(m.controller, "Station", []string{"FIXED", "MOBILE", "PORTABLE", "ROVER", "ROVER-LIMITED", "ROVER-UNLIMITED", "EXPEDITION", "HQ", "SCHOOL"})
	switch co {
	case "HIGH":
		cl.CategoryPower = cabrillo.CategoryPowerHigh
	case "LOW":
		cl.CategoryPower = cabrillo.CategoryPowerLow
	default:
		cl.CategoryPower = cabrillo.CategoryPowerQRP
	}
	if !ok {
		return
	}

	co, ok = ui.InputChoice(m.controller, "Overlay", []string{"", "CLASSIC", "ROOKIE", "TB-WIRES", "NOVICE-TECH", "OVER-50"})
	switch co {
	case "":
		cl.CategoryOverlay = cabrillo.CategoryOverlayUnknown
	case "CLASSIC":
		cl.CategoryOverlay = cabrillo.CategoryOverlayClassic
	case "ROOKIE":
		cl.CategoryOverlay = cabrillo.CategoryOverlayRookie
	case "TB-WIRES":
		cl.CategoryOverlay = cabrillo.CategoryOverlayTBWires
	case "NOVICE-TECH":
		cl.CategoryOverlay = cabrillo.CategoryOverlayNoviceTech
	case "OVER-50":
		cl.CategoryOverlay = cabrillo.CategoryOverlayOver50
	}
	if !ok {
		return
	}

	cl.ClaimedScore, ok = ui.InputInteger(m.controller, "Claimed Score")

	cl.Operators = m.cfg.Operator.Call
	for _, v := range m.alog.Records() {
		cl.QSOS = append(cl.QSOS, AdifToCabrillo(v, m.cfg))
	}
	cl.WriteToFile(exportFilename)
}

func AdifToCabrillo(v adif.Record, cfg *Config) cabrillo.QSO {
	qso := cabrillo.QSO{}

	freq := int(v.GetFloat(adif.Frequency) * 1e3)
	qso.Frequency = strconv.Itoa(freq)
	switch v.Get(adif.AMode) {
	case "SSB":
		qso.Mode = "PH"
	default:
		qso.Mode = v.Get(adif.AMode)
	}

	t, err := v.GetTimeOn()
	if err != nil {
		log.Printf("error parsing time on: %s", err)
	}
	qso.Timestamp = t
	qso.SentCall = cfg.Operator.Call
	qso.SentRST = v.Get(adif.RSTSent)
	qso.SentExchange = v.Get(adif.SRXString)
	qso.RcvdCall = v.Get(adif.Call)
	qso.RcvdRST = v.Get(adif.RSTReceived)
	qso.RcvdExchange = v.Get(adif.STXString)
	return qso
}
func (m *mainScreen) tuneLeft() {
	freq, err := m.rig.GetFreq(goHamlib.VFOCurrent)
	if err == nil {
		freq -= 500
		m.rig.SetFreq(goHamlib.VFOCurrent, freq)
	}
}
func (m *mainScreen) tuneRight() {
	freq, err := m.rig.GetFreq(goHamlib.VFOCurrent)
	if err == nil {
		freq += 500
		m.rig.SetFreq(goHamlib.VFOCurrent, freq)
	}
}

func (m *mainScreen) redrawAll() {
	w, h := termbox.Size()
	ui.Clear(0, 0, w, h, termbox.ColorDefault, termbox.ColorDefault)
	termbox.Flush()
	m.Tick()
}

func (m *mainScreen) commitLog() {
	if m.repo == nil {
		m.logErrorf("Log directory is not a git repository")
		return
	}
	if m.repo != nil {
		commitMsg, ok := ui.InputString(m.controller, "Commit Comment")
		if !ok {
			return
		}
		m.commitLogWithMessage(commitMsg)
	}
}

func (m *mainScreen) commitLogWithMessage(commitMsg string) {
	// ham logs are being stored in a git repo
	wt, err := m.repo.Worktree()
	if err == nil {
		fileNameInRepo := m.alog.Filename
		cfg, _ := m.repo.Config()
		// the git library wants a relative name
		if cfg != nil && strings.HasPrefix(fileNameInRepo, wt.Filesystem.Root()) {
			fileNameInRepo = fileNameInRepo[len(wt.Filesystem.Root())+1:]
		}

		st, err := wt.Status()
		if err != nil {
			m.logErrorf("unable to add log to repo: %s", err)
			return
		}

		ci, err := m.repo.Log(&git.LogOptions{
			FileName: &fileNameInRepo,
		})

		// easiest way I've found to determine if the file is already in the repo is to check it's
		// commit history
		existsInRepo := false
		ci.ForEach(func(c *object.Commit) error {
			existsInRepo = true
			return errors.New("finished")
		})

		fs := st.File(fileNameInRepo)

		// file exists in repo already, but is not modified, so don't commit
		if fs.Worktree != git.Modified && fs.Worktree != git.Added && existsInRepo {
			m.logInfo("current log not modified, skipping commit")
			return
		}

		_, err = wt.Add(fileNameInRepo)
		if err != nil {
			m.logErrorf("unable to add log to repo: %s", err)
			return
		}

		// commit the log
		_, err = wt.Commit(commitMsg, &git.CommitOptions{
			Author: &object.Signature{
				Name:  m.cfg.Operator.Name,
				Email: m.cfg.Operator.Email,
				When:  time.Now(),
			}})
		if err != nil {
			m.logErrorf("unable to add commit to repo: %s", err)
			return
		}

		succMsg := fmt.Sprintf("committed %s to repo", fileNameInRepo)
		// do we need to push it?
		if m.cfg.Operator.GitPushAfterCommit && !m.cfg.noNet {
			po := &git.PushOptions{}
			po.Auth, _ = m.cfg.GitAuth()
			err = m.repo.Push(po)
			if err != nil && err != git.NoErrAlreadyUpToDate {
				m.logErrorf("unable to push repository: %s", err)
				return
			}
			succMsg = fmt.Sprintf("committed %s to repo and pushed", fileNameInRepo)
		}
		m.logInfo(succMsg)
	}
	return
}

func (m *mainScreen) saveBookmark() {
	b := ham.Bookmark{}
	b.Created = time.Now()
	b.Frequency = m.qso.FrequencyValue()
	notes, ok := ui.InputString(m.controller, fmt.Sprintf("Notes for %f", b.Frequency))
	if !ok {
		return
	}
	b.Notes = notes

	mode, w, err := m.rig.GetMode(goHamlib.VFOCurrent)
	if err == nil {
		b.Mode = ham.BookmarkMode(mode)
		b.Width = w
	}

	m.bookmarks.AddBookmark(b)
	if err := m.bookmarks.Save(); err != nil {
		m.logErrorf("unable to save bookmarks: %s", err)
	}

}

func (m *mainScreen) listBookmarks() {
	bml := ui.NewBookmarkList(5, m.bookmarks, 20, m.cfg.Theme)
	pc := ui.NewPanelController(m.cfg.Theme)
	pc.AddWidget(bml)
	pc.Focus(bml)
lfor:
	for {
		pc.Redraw()
		termbox.Flush()
		ev := input.ReadKeyEvent()
		switch ev {
		case input.KeyEscape:
			break lfor
		case input.KeyEnter:
			idx := bml.Selected()
			if idx >= 0 && idx < len(m.bookmarks.Bookmark) {
				bm := m.bookmarks.Bookmark[idx]
				m.qso.SetFrequency(bm.Frequency * 1e6)
				if bm.Width != 0 && goHamlib.Mode(bm.Mode) != goHamlib.ModeNONE {
					m.rig.SetMode(goHamlib.VFOCurrent, goHamlib.Mode(bm.Mode), bm.Width)
				}
			}
			break lfor
		case input.KeyDelete:
			idx := bml.Selected()
			if idx >= 0 && idx < len(m.bookmarks.Bookmark) {
				m.bookmarks.RemoveAt(idx)
				m.bookmarks.Save()
			}
		default:
			pc.HandleEvent(ev)
		}
	}
}

func (m *mainScreen) newQSO() {
	result := ui.YesNoQuestion("Create New Contact?")
	if result {
		m.qso.SetDefaults()
		m.editingQSO = false
		m.controller.Focus(m.qso)
	}
}

func (m *mainScreen) focusQSOList() {
	m.controller.Focus(m.qsoList)
}

func (m *mainScreen) saveQSO() {
	if m.qso.IsValid() || ui.YesNoQuestion("Missing callsign or frequency, save anyway?") {
		rec := m.qso.GetRecord()
		if m.editingQSO {
			idx := m.qsoList.SelectedIndex()
			m.alog.ReplaceRecord(idx, rec)
			m.alog.Save()
		} else {
			m.toBeLogged <- logRequest{record: rec.Copy()}

			m.qso.SetDefaults()
			m.controller.Focus(m.qso)
		}
	}
}

func (m *mainScreen) showConditions() {
	sb := strings.Builder{}

	formatDate := m.solar.SolarData.Updated.Format("01-02-2006 15:04:05")
	sb.WriteString(fmt.Sprintf("%s UTC\n", formatDate))
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("Sunspots:     %d\n", m.solar.SolarData.Sunspots))
	sb.WriteString(fmt.Sprintf("Solar Flux:   %d\n", m.solar.SolarData.SolarFlux))
	sb.WriteString(fmt.Sprintf("MUF:          %0.2f MHz\n", m.solar.SolarData.MUF))
	sb.WriteString(fmt.Sprintf("K Index:      %d\n", m.solar.SolarData.KIndex))
	sb.WriteString(fmt.Sprintf("A Index:      %d\n", m.solar.SolarData.AIndex))
	sb.WriteString(fmt.Sprintf("Signal Noise: %s\n", m.solar.SolarData.SignalNoise))
	sb.WriteString("\n")
	for _, band := range m.solar.SolarData.CalculatedConditions.Bands {
		// Add spaces to day to make as long as night
		timeStr := band.Time
		spacesToAdd := 5 - len(timeStr)
		for i := 0; i < spacesToAdd; i++ {
			timeStr += " "
		}
		sb.WriteString(fmt.Sprintf("%s %s\t%s", band.Name, timeStr, band.Condition))
		sb.WriteString("\n")
	}

	sb.WriteString("\n")
	sb.WriteString("Press ESC to close")
	ui.Splash("Conditions", sb.String())

}

func (m *mainScreen) showHelp() {
	sb := strings.Builder{}
	sb.WriteString("Ctrl+H - Show Help           Ctrl+Q - Quit\n")
	sb.WriteString("\n")

	sb.WriteString("QSO\n")
	sb.WriteString("Ctrl+N    - New QSO\n")
	sb.WriteString("Ctrl+S    - Save QSO\n")
	sb.WriteString("Ctrl+D    - Set Date/Time on QSO to current time\n")
	sb.WriteString("Ctrl+L    - Focus QSO List\n")
	sb.WriteString("\n")
	sb.WriteString("Bookmarks\n")
	sb.WriteString("Ctrl+B    - Bookmark Current Frequency\n")
	sb.WriteString("Alt+B     - Display Bookmarks\n")
	sb.WriteString("\n")
	sb.WriteString("Misc\n")
	sb.WriteString("Ctrl+E    - Display Custom Commands\n")
	sb.WriteString("Ctrl+G    - Commit log file to git\n")
	sb.WriteString("Ctrl+R    - Force Screen Redraw\n")
	sb.WriteString("Alt+Left  - Tune Down\n")
	sb.WriteString("Alt+Right - Tune Up\n")
	sb.WriteString("Ctrl+C    - Show Band Conditions\n")
	sb.WriteString("\n")
	sb.WriteString("Press ESC to close")
	ui.Splash("Commands", sb.String())
}

func (m *mainScreen) Tick() bool {
	m.controller.Redraw()

	m.pollForLogs()

	if !m.controller.HandleEvent(input.ReadKeyEvent()) {
		// shutting down the UI, so turn the console logger back on
		log.SetFlags(log.LstdFlags)
		log.SetOutput(os.Stderr)
		m.loggingReplaced = false

		m.controller.Shutdown()
		if m.wsjtxLog != nil {
			m.wsjtxLog.Close()
		}
		if m.fldigiLog != nil {
			m.fldigiLog.Close()
		}
		if m.js8log != nil {
			m.js8log.Close()
		}
		if m.cfg.Operator.GitCommitOnExit {
			m.commitLogWithMessage("auto-commit on exit")
		}
		close(m.shutdown)
		return false
	}
	return true
}

func (m *mainScreen) pollForLogs() {
	if m.cfg.WSJTX.Enabled {
		select {
		case msg := <-m.wsjtxLog.Messages:
			switch v := msg.(type) {
			case *logingest.WSJTXQSOLogged:
				arec, err := convertToADIF(v)
				if err != nil {
					m.logErrorf("error converting QSO: %s", err)
				} else {
					m.logInfo("received QSO from WSJT-X: %s %s", arec.Get(adif.Call), arec.Get(adif.AMode))
					m.toBeLogged <- logRequest{record: arec, external: true}
				}
			}
		default:
		}
	}
	if m.cfg.FLLog.Enabled {
		select {
		case rec := <-m.fldigiLog.Messages:
			rdr := strings.NewReader("<eoh>\n" + rec)
			alog, err := adif.Parse(rdr)
			if err == nil && alog.NumRecords() == 1 {
				arec, _ := alog.GetRecord(0)
				m.logInfo("received QSO from fldigi: %s %s", arec.Get(adif.Call), arec.Get(adif.AMode))
				m.toBeLogged <- logRequest{record: arec, external: true}
			}
		default:
		}
	}
	if m.cfg.JS8Call.Enabled {
		select {
		case rec := <-m.js8log.Messages:
			if rec.Type == "LOG.QSO" {
				rdr := strings.NewReader("<eoh>\n" + rec.Value)
				alog, err := adif.Parse(rdr)
				if err == nil && alog.NumRecords() == 1 {
					arec, _ := alog.GetRecord(0)
					m.logInfo("received QSO from JS8Call: %s %s", arec.Get(adif.Call), arec.Get(adif.AMode))
					m.toBeLogged <- logRequest{record: arec, external: true}
				}
			}
		default:
		}
	}
}

func (m *mainScreen) logErrorf(s string, a ...interface{}) {
	if m.loggingReplaced {
		msg := fmt.Sprintf(s, a...)
		m.messages.AddError(msg)
	} else {
		log.Printf(s, a...)
	}
}

func (m *mainScreen) logInfo(s string, a ...interface{}) {
	if m.loggingReplaced {
		msg := fmt.Sprintf(s, a...)
		m.messages.AddMessage(msg)
	} else {
		log.Printf(s, a...)
	}
}

func (m *mainScreen) executeCommands() {
	_, h := termbox.Size()
	cml := ui.NewCommandList(5, m.cfg.Operator.Commands, h-10, m.cfg.Theme)
	pc := ui.NewPanelController(m.cfg.Theme)
	pc.AddWidget(cml)
	pc.Focus(cml)

	execute := func(cmd ui.Command) {
		start := time.Now()
		ec := exec.Command("bash", "-c", cmd.Command)
		ec.Env = os.Environ()
		ec.Env = append(ec.Env, fmt.Sprintf("TQSL=%s", m.cfg.Operator.LOTWtqslPath))
		ec.Env = append(ec.Env, fmt.Sprintf("LOGFILE=%s", m.alog.Filename))
		op, err := ec.CombinedOutput()
		if err != nil {
			if len(op) > 0 {
				m.logErrorf("error executing %s [%s]: %s", cmd.Name, err, string(op))
			} else {
				m.logErrorf("error executing %s [%s]", cmd.Name, err)
			}
		} else {
			took := time.Now().Sub(start)
			if len(op) > 0 {
				m.logInfo("executed %s (took %s): %s", cmd.Name, took, string(op))
			} else {
				m.logInfo("executed %s (took %s)", cmd.Name, took)
			}
		}
	}
lfor:
	for {
		pc.Redraw()
		termbox.Flush()
		ev := input.ReadKeyEvent()
		switch ev {
		case input.KeyEscape:
			break lfor
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			idx := int(ev) - 49
			if idx >= 0 && idx < len(m.cfg.Operator.Commands) {
				cmd := m.cfg.Operator.Commands[idx]
				execute(cmd)
				break lfor
			}
		case input.KeyEnter:
			idx := cml.Selected()
			if idx >= 0 && idx < len(m.cfg.Operator.Commands) {
				cmd := m.cfg.Operator.Commands[idx]
				execute(cmd)
			}
			break lfor
		default:
			pc.HandleEvent(ev)
		}
	}
}

func (m *mainScreen) logToLOTW(rec adif.Record) adif.Record {
	lc := logsync.NewLOTWClient(m.cfg.Operator.LOTWUsername, m.cfg.Operator.LOTWPassword, m.cfg.Operator.LOTWtqslPath)
	if err := lc.UploadQSO(rec); err != nil {
		m.logErrorf("error uploading LoTW QSO: %s", err)
	} else {
		m.logInfo("sent %s QSO to LoTW", rec.Get(adif.Call))
		rec = append(rec, adif.Field{
			Name:  adif.LOTWSent,
			Value: "Y",
		})
		rec = append(rec, adif.Field{
			Name:  adif.LOTWSentDate,
			Value: time.Now().Format("20060102"),
		})
	}
	return rec
}

func (m *mainScreen) handleHamlibDebug(level goHamlib.DebugLevel, msg string) {
	switch level {
	case goHamlib.DebugWarn, goHamlib.DebugErr, goHamlib.DebugBug:
		m.logErrorf("hamlib error: %s", msg)
	case goHamlib.DebugTrace, goHamlib.DebugVerbose:
		m.logInfo("hamlib: %s", msg)
	}
}

func convertToADIF(msg *logingest.WSJTXQSOLogged) (adif.Record, error) {
	record := adif.Record{}

	record = append(record,
		adif.Field{
			Name:  adif.QSODateStart,
			Value: adif.UTCDate(msg.QSOOn),
		})
	record = append(record,
		adif.Field{
			Name:  adif.TimeOn,
			Value: adif.UTCTime(msg.QSOOn),
		})

	record = append(record,
		adif.Field{
			Name:  adif.QSODateEnd,
			Value: adif.UTCDate(msg.QSOOff),
		})
	record = append(record,
		adif.Field{
			Name:  adif.TimeOff,
			Value: adif.UTCTime(msg.QSOOff),
		})

	record = append(record,
		adif.Field{
			Name:  adif.Call,
			Value: msg.DXCall,
		})
	record = append(record,
		adif.Field{
			Name:  adif.AMode,
			Value: msg.Mode,
		})
	record = append(record,
		adif.Field{
			Name:  adif.Frequency,
			Value: strconv.FormatFloat(msg.Frequency, 'f', -1, 64),
		})
	b, found := adif.DetermineBand(msg.Frequency)
	if found {
		record = append(record,
			adif.Field{
				Name:  adif.ABand,
				Value: b.Name,
			})
	}

	record = append(record,
		adif.Field{
			Name:  adif.RSTSent,
			Value: msg.RST,
		})

	record = append(record,
		adif.Field{
			Name:  adif.RSTReceived,
			Value: msg.RRT,
		})

	record = append(record,
		adif.Field{
			Name:  adif.GridSquare,
			Value: msg.DXGrid,
		})
	record = append(record,
		adif.Field{
			Name:  adif.Name,
			Value: msg.Name,
		})

	record = append(record,
		adif.Field{
			Name:  adif.Comment,
			Value: msg.Comments,
		})

	record = append(record,
		adif.Field{
			Name:  adif.TXPower,
			Value: msg.TXPower,
		})

	return record, nil
}
