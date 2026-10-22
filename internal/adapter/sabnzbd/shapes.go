package sabnzbd

// The JSON shapes below follow SABnzbd's: numbers that SAB sends as strings
// ("mb", "mbleft", "percentage", "kbpersec", "diskspace1", ...) are strings
// here too, because Sonarr and Radarr deserialize them as such.

// errorBody is the shape of every failure: HTTP 200 with status false.
type errorBody struct {
	Status bool   `json:"status"`
	Error  string `json:"error"`
}

// okBody is the answer to a mode that only reports success.
type okBody struct {
	Status bool `json:"status"`
}

// idsBody answers addfile, addurl and queue deletes.
type idsBody struct {
	Status bool     `json:"status"`
	NzoIDs []string `json:"nzo_ids"`
}

type versionBody struct {
	Version string `json:"version"`
}

type categoriesBody struct {
	Categories []string `json:"categories"`
}

type configBody struct {
	Config config `json:"config"`
}

type config struct {
	Misc       configMisc       `json:"misc"`
	Categories []configCategory `json:"categories"`
	Servers    []struct{}       `json:"servers"`
	Sorters    []struct{}       `json:"sorters"`
}

type configMisc struct {
	CompleteDir            string   `json:"complete_dir"`
	DownloadDir            string   `json:"download_dir"`
	EnableTVSorting        bool     `json:"enable_tv_sorting"`
	EnableMovieSorting     bool     `json:"enable_movie_sorting"`
	EnableDateSorting      bool     `json:"enable_date_sorting"`
	TVCategories           []string `json:"tv_categories"`
	MovieCategories        []string `json:"movie_categories"`
	DateCategories         []string `json:"date_categories"`
	PreCheck               bool     `json:"pre_check"`
	HistoryRetention       string   `json:"history_retention"`
	HistoryRetentionOption string   `json:"history_retention_option"`
	HistoryRetentionNumber int      `json:"history_retention_number"`
}

type configCategory struct {
	Name     string `json:"name"`
	Order    int    `json:"order"`
	PP       string `json:"pp"`
	Script   string `json:"script"`
	Dir      string `json:"dir"`
	Newzbin  string `json:"newzbin"`
	Priority int    `json:"priority"`
}

type fullStatusBody struct {
	Status fullStatus `json:"status"`
}

type fullStatus struct {
	Version      string   `json:"version"`
	CompleteDir  string   `json:"completedir"`
	DownloadDir  string   `json:"downloaddir"`
	Paused       bool     `json:"paused"`
	PauseInt     string   `json:"pause_int"`
	HaveWarnings string   `json:"have_warnings"`
	Warnings     []string `json:"warnings"`
	DiskSpace1   string   `json:"diskspace1"`
	DiskSpace2   string   `json:"diskspace2"`
}

type queueBody struct {
	Queue queue `json:"queue"`
}

type queue struct {
	Version         string      `json:"version"`
	Paused          bool        `json:"paused"`
	PauseInt        string      `json:"pause_int"`
	PausedAll       bool        `json:"paused_all"`
	Status          string      `json:"status"`
	SpeedLimit      string      `json:"speedlimit"`
	SpeedLimitAbs   string      `json:"speedlimit_abs"`
	Speed           string      `json:"speed"`
	KBPerSec        string      `json:"kbpersec"`
	Size            string      `json:"size"`
	SizeLeft        string      `json:"sizeleft"`
	MB              string      `json:"mb"`
	MBLeft          string      `json:"mbleft"`
	TimeLeft        string      `json:"timeleft"`
	NoOfSlots       int         `json:"noofslots"`
	NoOfSlotsTotal  int         `json:"noofslots_total"`
	Start           int         `json:"start"`
	Limit           int         `json:"limit"`
	Finish          int         `json:"finish"`
	DiskSpace1      string      `json:"diskspace1"`
	DiskSpace2      string      `json:"diskspace2"`
	DiskSpaceTotal1 string      `json:"diskspacetotal1"`
	DiskSpaceTotal2 string      `json:"diskspacetotal2"`
	DiskSpace1Norm  string      `json:"diskspace1_norm"`
	DiskSpace2Norm  string      `json:"diskspace2_norm"`
	HaveWarnings    string      `json:"have_warnings"`
	CacheArt        string      `json:"cache_art"`
	CacheSize       string      `json:"cache_size"`
	FinishAction    *string     `json:"finishaction"`
	Quota           string      `json:"quota"`
	HaveQuota       bool        `json:"have_quota"`
	LeftQuota       string      `json:"left_quota"`
	Slots           []queueSlot `json:"slots"`
}

type queueSlot struct {
	Status     string   `json:"status"`
	Index      int      `json:"index"`
	NzoID      string   `json:"nzo_id"`
	Filename   string   `json:"filename"`
	Cat        string   `json:"cat"`
	Priority   string   `json:"priority"`
	Script     string   `json:"script"`
	MB         string   `json:"mb"`
	MBLeft     string   `json:"mbleft"`
	MBMissing  string   `json:"mbmissing"`
	Size       string   `json:"size"`
	SizeLeft   string   `json:"sizeleft"`
	Percentage string   `json:"percentage"`
	TimeLeft   string   `json:"timeleft"`
	AvgAge     string   `json:"avg_age"`
	Password   string   `json:"password"`
	UnpackOpts string   `json:"unpackopts"`
	HasRating  bool     `json:"has_rating"`
	Labels     []string `json:"labels"`
}

type historyBody struct {
	History history `json:"history"`
}

type history struct {
	NoOfSlots         int           `json:"noofslots"`
	PPSlots           int           `json:"ppslots"`
	TotalSize         string        `json:"total_size"`
	MonthSize         string        `json:"month_size"`
	WeekSize          string        `json:"week_size"`
	DaySize           string        `json:"day_size"`
	LastHistoryUpdate int64         `json:"last_history_update"`
	Slots             []historySlot `json:"slots"`
}

type historySlot struct {
	NzoID        string `json:"nzo_id"`
	Name         string `json:"name"`
	NzbName      string `json:"nzb_name"`
	Category     string `json:"category"`
	Status       string `json:"status"`
	FailMessage  string `json:"fail_message"`
	ActionLine   string `json:"action_line"`
	Storage      string `json:"storage"`
	Path         string `json:"path"`
	Bytes        int64  `json:"bytes"`
	Size         string `json:"size"`
	Downloaded   int64  `json:"downloaded"`
	Completed    int64  `json:"completed"`
	DownloadTime int64  `json:"download_time"`
	PostprocTime int64  `json:"postproc_time"`
	PP           string `json:"pp"`
	Script       string `json:"script"`
	ScriptLine   string `json:"script_line"`
	Report       string `json:"report"`
	Password     string `json:"password"`
	Retry        int    `json:"retry"`
	Loaded       bool   `json:"loaded"`
	HasRating    bool   `json:"has_rating"`
	URL          string `json:"url"`
	URLInfo      string `json:"url_info"`
	MD5Sum       string `json:"md5sum"`
	DuplicateKey string `json:"duplicate_key"`
}

type positionBody struct {
	Status   bool `json:"status"`
	Position int  `json:"position"`
}
