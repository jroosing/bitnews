package web

import (
	"net/http"

	"github.com/a-h/templ"

	"github.com/jroosing/bitnews/internal/core"
)

// postprocessingSection is the "Post-processing" entry of the settings
// registry (M9-03): the two TRaSH script options of ADR-0009.
func postprocessingSection() SettingsSection {
	return SettingsSection{
		Slug: "postprocessing", Title: "Post-processing", Icon: "check-circle", Order: 35,
		Description: "Optional tidying of the names of NZB downloads.",
		Component:   SettingsPostprocessingPage, Data: settingsPostprocessingData, Routes: (*Server).registerPostprocessingRoutes,
	}
}

func settingsPostprocessingData(s *Server, r *http.Request) (any, error) {
	return s.cardPage(r, postprocessingCards, nil), nil
}

func (s *Server) registerPostprocessingRoutes() { s.registerCards(postprocessingCards) }

var postprocessingCards = []*settingsCard{postprocessingNamesCard}

// postprocessingNamesCard edits Settings.PostProcess. Both options are read
// when a download is added or finished, so they apply to the next download.
var postprocessingNamesCard = &settingsCard{
	key: "names", route: "/settings/postprocessing", slug: "postprocessing",
	render: func(view any) templ.Component { return PostprocessingNamesCard(view.(cardView)) },
	saved:  "Post-processing options saved. They apply to the next download.",
	values: func(s core.Settings) map[string]string {
		return map[string]string{
			"clean_names":         checked(s.PostProcess.CleanNames),
			"underscores_to_dots": checked(s.PostProcess.UnderscoresToDots),
		}
	},
	read: func(_ *Server, f map[string]string, next *core.Settings) map[string]string {
		next.PostProcess.CleanNames = on(f, "clean_names")
		next.PostProcess.UnderscoresToDots = on(f, "underscores_to_dots")
		return nil
	},
}
