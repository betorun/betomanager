// Package llm : accès aux modèles de langage pour les fonctions d'IA de
// betorun (le Copilot en premier). Rien d'autre dans le code n'appelle un
// fournisseur directement.
//
// # Contrat
//
// Un Provider prend une Request (prompt système, conversation, outils) et rend
// une Response, en une fois (Generate) ou en flux (Stream). L'appel d'outils est
// NATIF : les outils partent en schémas JSON, les appels reviennent structurés
// (ToolCall). Le texte du modèle n'est jamais analysé pour y chercher des
// appels : c'est la principale fragilité du Copilot SYK, qu'on ne reproduit pas
// (docs/adr/0008-llm-provider-layer.md).
//
// Tout appel d'outil rendu par un Provider est déjà vérifié (CheckToolCalls) :
// nom non vide et déclaré dans la requête, arguments en objet JSON valide.
// Sinon : *Error de type KindMalformedOutput, que la chaîne de bascule traite
// comme une panne du fournisseur.
//
// # Composition
//
// Les adaptateurs (sous-paquet openai) ne parlent qu'au fournisseur. Le reste
// s'empile en décorateurs, chacun avec une seule responsabilité :
//
//		Instrument( Chain( Breaker(Budget(openai A)), Breaker(Budget(openai B)) ) )
//
//	  - Budget  : réserve le coût maximal avant l'appel, règle le coût réel après
//	    (le trop-perçu est rendu), plafond par organisation et par jour ;
//	  - Breaker : coupe un fournisseur après N pannes de suite, le réessaie après
//	    un délai ;
//	  - Chain   : bascule vers le fournisseur suivant sur une panne qui le permet
//	    (jamais après le premier événement d'un flux déjà transmis) ;
//	  - Instrument : métriques Prometheus, sans label à cardinalité ouverte.
//
// llmsetup assemble cette pile depuis la configuration.
//
// # Ce qui ne part jamais chez un fournisseur
//
// Request.Caller (organisation, utilisateur, tour) sert au budget et aux
// journaux, jamais au corps de la requête. Les valeurs des secrets des
// conteneurs n'ont rien à faire dans un prompt : c'est à l'appelant (le
// Copilot) de ne pas les y mettre.
package llm
