package qbit

// routes is the table of supported calls, keyed by the path below /api/v2/.
// Both the names of qBittorrent before 5.0 (pause, resume) and since (stop,
// start) are accepted.
func routes() map[string]endpoint {
	return map[string]endpoint{
		"auth/login":  {post: true, public: true, fn: (*call).login},
		"auth/logout": {post: true, fn: (*call).logout},

		"app/version":        {fn: (*call).version},
		"app/webapiVersion":  {fn: (*call).webAPIVersion},
		"app/preferences":    {fn: (*call).getPreferences},
		"app/setPreferences": {post: true, fn: (*call).setPreferences},

		"torrents/info":       {fn: (*call).info},
		"torrents/properties": {fn: (*call).properties},
		"torrents/files":      {fn: (*call).files},
		"torrents/tags":       {fn: (*call).tags},
		"sync/maindata":       {fn: (*call).maindata},

		"torrents/add":            {post: true, upload: true, fn: (*call).add},
		"torrents/pause":          {post: true, fn: (*call).pause},
		"torrents/stop":           {post: true, fn: (*call).pause},
		"torrents/resume":         {post: true, fn: (*call).resume},
		"torrents/start":          {post: true, fn: (*call).resume},
		"torrents/delete":         {post: true, fn: (*call).delete},
		"torrents/setCategory":    {post: true, fn: (*call).setCategory},
		"torrents/setShareLimits": {post: true, fn: (*call).setShareLimits},
		"torrents/setForceStart":  {post: true, fn: (*call).setForceStart},
		"torrents/topPrio":        {post: true, fn: (*call).topPrio},
		"torrents/bottomPrio":     {post: true, fn: (*call).bottomPrio},
		"torrents/increasePrio":   {post: true, fn: (*call).increasePrio},
		"torrents/decreasePrio":   {post: true, fn: (*call).decreasePrio},

		"torrents/categories":     {fn: (*call).categories},
		"torrents/createCategory": {post: true, fn: (*call).createCategory},
	}
}
