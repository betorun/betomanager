package workflows

import "time"

// Noms des workflows Temporal (nom de la fonction enregistrée par le worker).
const (
	CreateProjectWorkflowName = "CreateProjectWorkflow"
	DeleteProjectWorkflowName = "DeleteProjectWorkflow"
	// UpdateProjectLimitsWorkflowName : quota Kubernetes après un changement
	// des limites du projet.
	UpdateProjectLimitsWorkflowName = "UpdateProjectLimitsWorkflow"
)

// Préfixes des identifiants de workflow (suivis de l'identifiant du projet).
const (
	WorkflowIDCreatePrefix = "project-create-"
	WorkflowIDDeletePrefix = "project-delete-"
	WorkflowIDLimitsPrefix = "project-limits-"
)

// Politique d'exécution des activities des workflows de projet.
const (
	activityStartToCloseTimeout      = 5 * time.Minute
	activityRetryInitialInterval     = time.Second
	activityRetryMaximumAttempts     = 3
	localActivityStartToCloseTimeout = 5 * time.Second
	// compensationTimeout : délai des activities de compensation (rollback).
	compensationTimeout = 1 * time.Minute
)
