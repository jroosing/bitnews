package web

// PathChecker is the file system seam of the paths settings.
type PathChecker = pathChecker

// SetPathChecker replaces the file system checks of s. Tests only.
func SetPathChecker(s *Server, c PathChecker) { s.pathCheck = c }
