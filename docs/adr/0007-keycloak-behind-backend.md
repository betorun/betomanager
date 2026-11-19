# ADR 0007 — Keycloak derrière le backend, invisible des utilisateurs

- **Status** : proposé
- **Date** : 2026-10-30
- **Amende** : [ADR 0005](0005-keycloak-source-of-truth.md)

## Contexte

L'ADR 0005 délègue tous les flux d'authentification à Keycloak, y compris ses pages (connexion, OTP, compte), ses emails et ses jetons, remis tels quels à la console et à la CLI.

Nouvelle exigence produit : la console ne doit jamais rien montrer de Keycloak, et personne ne doit pouvoir deviner qu'il gère les comptes. Seul Next.js affiche des pages, seul le backend parle à Keycloak. betorun vise à devenir un fournisseur cloud : l'authentification fait partie du produit et de sa marque.

L'ADR 0005 citait déjà cette option : « écrire notre propre UI et utiliser Keycloak en pur backend ».

## Décision

- **Keycloak reste l'annuaire** : comptes et mots de passe (betorun ne stocke toujours aucun mot de passe). Il n'est joignable que par le backend, sur le réseau privé.
- **Le backend est la façade d'authentification** : connexion, sessions révocables, jetons betorun (signés par les clés du STS), MFA (TOTP, codes de secours, puis passkeys), emails, OAuth Google/GitHub en direct, autorisation de la CLI.
- **Next.js affiche toutes les pages**, session web en cookie `HttpOnly`.
- Keycloak est derrière une interface `Directory` remplaçable.

Détails : [docs/architecture/auth.md](../architecture/auth.md).

## Conséquences

### Positives

- Marque et parcours entièrement maîtrisés (FR/EN, design betorun).
- Keycloak n'est plus exposé sur Internet : surface d'attaque réduite.
- Sessions de niveau fournisseur cloud : liste, révocation, détection de vol de jeton de rafraîchissement.
- Les jetons ne révèlent plus l'infrastructure ; un seul format de jeton (humains et machines) pour le middleware.
- Keycloak devient remplaçable.

### Négatives

- Plus de code de sécurité à maintenir : sessions, MFA, emails, OAuth social. À compenser par des bibliothèques éprouvées, des tests et l'audit.
- Le backend vérifie les mots de passe via le grant « password » de Keycloak, en interne. Acceptable parce qu'il n'est jamais offert à un client, mais à garder sous un client confidentiel dédié et sur le réseau privé.
- Emails transactionnels à opérer (délivrabilité).

## Alternatives considérées

- **Thème Keycloak aux couleurs de betorun** : les pages restent servies par Keycloak (domaine, comportements, erreurs, emails) ; contraire à l'exigence.
- **Remplacer Keycloak par une auth maison complète** (mots de passe compris) : plus de risque immédiat sans bénéfice pour l'exigence. Reste possible plus tard grâce à l'interface `Directory`.
- **Ory Kratos** (auth sans interface, pages à fournir) : bien adapté, mais une migration de plus ; à reconsidérer si Keycloak devient un poids.
