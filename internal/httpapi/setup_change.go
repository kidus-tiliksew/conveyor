package httpapi

import "net/http"

func (s *Server) changeTaskSetup(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusGone, map[string]string{"error": "execution_configuration_retired", "message": "task setup changes are retired; revise the frozen pipeline policy instead"})
}
