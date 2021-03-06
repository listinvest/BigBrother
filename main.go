package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/Dirbaio/BigBrother/mpd"
	. "github.com/Dirbaio/BigBrother/mpd/helpers/ptrs"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

var storagePath = os.Getenv("BB_STORAGE_PATH")

var db *sqlx.DB

type Camera struct {
	ID     int    `db:"id" json:"id"`
	Name   string `db:"name" json:"name"`
	Source string `db:"source" json:"-"`
}

func (c *Camera) String() string {
	return fmt.Sprintf("%d/%s", c.ID, c.Name)
}

type Period struct {
	ID int `db:"id"`

	CameraID int `db:"camera_id"`

	MimeType  *string    `db:"mime_type"`
	Codecs    string     `db:"codecs"`
	Width     int        `db:"width"`
	Height    int        `db:"height"`
	Timescale int        `db:"timescale"`
	FrameRate string     `db:"frame_rate"`
	Time      *time.Time `db:"time"`
}

type Segment struct {
	ID int `db:"id"`

	CameraID int `db:"camera_id"`
	PeriodID int `db:"period_id"`

	Index  int        `db:"index"` // Index of segment within the period. Starts with 1
	Offset int64      `db:"off"`   // Offset of segment from the period start, in timescale units
	Length int64      `db:"len"`   // Length of segment in timescale units
	Time   *time.Time `db:"time"`  // Real time of the start of the segment
}

func getCamera(id int) (*Camera, error) {
	camera := Camera{}

	err := db.Get(&camera, "SELECT * FROM camera WHERE id=$1", id)
	if err != nil {
		return nil, err
	}

	return &camera, nil
}

func getCameras() ([]*Camera, error) {
	var res []*Camera

	err := db.Select(&res, "SELECT * FROM camera ORDER BY id ASC")
	if err != nil {
		return nil, err
	}

	return res, nil
}

func listCameras(rw http.ResponseWriter, req *http.Request) {
	res, err := getCameras()
	if err != nil {
		panic(err)
	}

	js, err := json.Marshal(res)
	if err != nil {
		panic(err)
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.Write(js)

}

func getMpd(rw http.ResponseWriter, req *http.Request) {
	cam, err := strconv.Atoi(req.URL.Query().Get("cam"))
	if err != nil {
		panic(err)
	}
	fromUnix, err := strconv.ParseInt(req.URL.Query().Get("from"), 10, 64)
	if err != nil {
		panic(err)
	}
	toUnix, err := strconv.ParseInt(req.URL.Query().Get("to"), 10, 64)
	if err != nil {
		panic(err)
	}
	from := time.Unix(fromUnix/1000, 0).UTC()
	to := time.Unix(toUnix/1000, 0).UTC()

	m := mpd.NewMPD(mpd.DASH_PROFILE_ONDEMAND, "PT1S")
	m.MediaPresentationDuration = nil
	m.Periods = []*mpd.Period{}

	segments := []Segment{}
	err = db.Select(&segments, "SELECT * FROM segment WHERE camera_id=$1 AND time BETWEEN $2 AND $3 ORDER BY period_id, id", cam, from, to)
	if err != nil {
		log.Fatal(err)
	}

	// TODO batch select of periods
	lastPeriodId := 0
	var stl *mpd.SegmentTimeline

	for i, segment := range segments {
		if segment.PeriodID != lastPeriodId {
			lastPeriodId = segment.PeriodID

			period := Period{}
			err := db.Get(&period, "SELECT * FROM period WHERE id=$1", segment.PeriodID)
			if err != nil {
				log.Fatal(err)
			}

			duration := int64(0)
			for j := i; j < len(segments) && segments[i].PeriodID == segments[j].PeriodID; j++ {
				duration += segments[j].Length
			}

			pt := period.Time.Add(time.Duration(segment.Offset * int64(1000000000) / int64(period.Timescale)))
			p := m.AddNewPeriod()
			p.SetDuration(time.Duration(duration * int64(1000000000) / int64(period.Timescale)))
			p.EventStreams = []*mpd.EventStream{
				&mpd.EventStream{
					SchemeIDURI: Strptr("bigbrother:realtime"),
					Value:       Strptr(pt.String()),
					Events: []*mpd.Event{
						&mpd.Event{
							PresentationTime: Intptr(0),
							Duration:         Intptr(1000000),
						},
					},
				},
			}
			as, _ := p.AddNewAdaptationSetVideo(mpd.DASH_MIME_TYPE_VIDEO_MP4, "progressive", true, 1)
			rep, _ := as.AddNewRepresentationVideo(1100690, period.Codecs, "0", period.FrameRate, int64(period.Width), int64(period.Height))
			initUrl := fmt.Sprintf("/stream/%d/%d/init-stream0.m4s", period.CameraID, period.ID)
			mediaUrl := fmt.Sprintf("/stream/%d/%d/chunk-stream0-$Number%%05d$.m4s", period.CameraID, period.ID)
			timescale := int64(period.Timescale)
			startNumber := int64(segment.Index)
			pto := uint64(segment.Offset)
			rep.SegmentTemplate = &mpd.SegmentTemplate{
				Timescale:              &timescale,
				Initialization:         &initUrl,
				Media:                  &mediaUrl,
				SegmentTimeline:        &mpd.SegmentTimeline{},
				StartNumber:            &startNumber,
				PresentationTimeOffset: &pto,
			}

			stl = rep.SegmentTemplate.SegmentTimeline
		}
		seg := mpd.SegmentTimelineSegment{
			Duration: uint64(segment.Length),
		}
		stl.Segments = append(stl.Segments, &seg)
	}

	mpdStr, _ := m.WriteToString()

	rw.Header().Add("Content-Type", "application/dash+xml")
	rw.WriteHeader(200)
	rw.Write([]byte(mpdStr))
}

var recorders []*Recorder

type indexWrapper struct {
	assets http.FileSystem
}

func (i *indexWrapper) Open(name string) (http.File, error) {
	ret, err := i.assets.Open(name)
	if !os.IsNotExist(err) || path.Ext(name) != "" {
		return ret, err
	}
	return i.assets.Open("index.html")
}

func main() {
	var err error
	db, err = sqlx.Connect("postgres", fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		os.Getenv("BB_POSTGRES_HOST"),
		os.Getenv("BB_POSTGRES_PORT"),
		os.Getenv("BB_POSTGRES_USER"),
		os.Getenv("BB_POSTGRES_PASSWORD"),
		os.Getenv("BB_POSTGRES_DATABASE"),
	))

	if err != nil {
		log.Fatalln(err)
	}

	fs := http.FileServer(&indexWrapper{http.Dir("static")})
	mux := http.NewServeMux()
	mux.Handle("/stream/", http.StripPrefix("/stream/", http.FileServer(http.Dir(storagePath))))
	mux.HandleFunc("/mpd", getMpd)
	mux.HandleFunc("/cameras", listCameras)
	mux.Handle("/", fs)

	log.Println("Listening...")

	var handler http.Handler = mux
	//handler = cors.New(cors.Options{Debug: true}).Handler(handler)
	go http.ListenAndServe(":3000", handler)

	cams, err := getCameras()
	if err != nil {
		log.Fatalln(err)
	}

	var wg sync.WaitGroup

	// Start all cams
	for _, cam := range cams {
		wg.Add(1)
		r := NewRecorder(cam)
		recorders = append(recorders, r)
		go func() {
			r.Run()
			wg.Done()
		}()
	}

	// Start janitor
	j := NewJanitor()
	wg.Add(1)
	go func() {
		j.Run()
		wg.Done()
	}()

	// Stop signal
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for range c {
			// Interrupt signal happened
			log.Print("Got sigint, exiting")
			for _, r := range recorders {
				r.Stop()
			}
			j.Stop()
			// sig is a ^C, handle it
		}
	}()

	wg.Wait()
	log.Print("All done, see you later")
}
