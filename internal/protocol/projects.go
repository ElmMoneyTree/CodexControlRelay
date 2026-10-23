package protocol

import "errors"

// Project IDs are opaque desktop identifiers; the device validates current
// availability again when it executes a create_thread command.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type ProjectCatalog struct {
	Projects []Project `json:"projects"`
	Backends []string  `json:"backends"`
	Error    string    `json:"error,omitempty"`
}

func DecodeProjectCatalog(data []byte) (ProjectCatalog, error) {
	var result ProjectCatalog
	if err := decodeOne(data, &result); err != nil {
		return result, err
	}
	if result.Error != "" {
		return ProjectCatalog{Error: "Projects are unavailable on the computer"}, nil
	}
	if result.Projects == nil || len(result.Projects) > 1000 || result.Backends == nil || len(result.Backends) > 2 {
		return result, errors.New("projects or backends are invalid")
	}
	backends := map[string]bool{}
	for _, backend := range result.Backends {
		if !oneOf(backend, "codex", "chatgpt") || backends[backend] {
			return result, errors.New("project backend is unsupported or duplicated")
		}
		backends[backend] = true
	}
	projects := map[string]bool{}
	for i := range result.Projects {
		project := &result.Projects[i]
		var err error
		if project.ID, err = required(project.ID, "projectId", 200); err != nil {
			return result, err
		}
		if project.Name, err = required(project.Name, "project name", 500); err != nil {
			return result, err
		}
		key := project.Kind + "\x00" + project.ID
		if !backends[project.Kind] || projects[key] {
			return result, errors.New("project kind is unsupported or project is duplicated")
		}
		projects[key] = true
	}
	return result, nil
}
