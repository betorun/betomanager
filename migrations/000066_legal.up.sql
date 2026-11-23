-- Documents juridiques versionnés et leur acceptation par les organisations
-- (docs/modules/legal.md) : conditions générales de vente des VPS (terms_vps)
-- et politique d'usage acceptable (aup).

CREATE TABLE IF NOT EXISTS legal_documents (
    kind          TEXT        NOT NULL,
    version       INTEGER     NOT NULL CHECK (version > 0),
    title_fr      TEXT        NOT NULL,
    title_en      TEXT        NOT NULL,
    body_fr       TEXT        NOT NULL,
    body_en       TEXT        NOT NULL,
    -- draft (modifiable) | published (immuable)
    status        TEXT        NOT NULL CHECK (status IN ('draft', 'published')),
    created_by    TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by    TEXT        NOT NULL DEFAULT '',
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_by  TEXT        NOT NULL DEFAULT '',
    published_at  TIMESTAMPTZ,
    PRIMARY KEY (kind, version),
    CHECK ((status = 'published') = (published_at IS NOT NULL))
);
-- Un seul brouillon à la fois par document.
CREATE UNIQUE INDEX IF NOT EXISTS uq_legal_documents_draft ON legal_documents (kind) WHERE status = 'draft';

-- Une version publiée ne change plus (ni texte, ni retour en brouillon, ni suppression).
CREATE OR REPLACE FUNCTION legal_documents_immutable() RETURNS trigger AS $$
BEGIN
    IF OLD.status = 'published' THEN
        RAISE EXCEPTION 'legal_documents: version % of % is published and immutable', OLD.version, OLD.kind;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_legal_documents_immutable ON legal_documents;
CREATE TRIGGER trg_legal_documents_immutable BEFORE UPDATE OR DELETE ON legal_documents
    FOR EACH ROW EXECUTE FUNCTION legal_documents_immutable();

-- Acceptations : preuve conservée (pas de suppression en cascade avec l'organisation).
CREATE TABLE IF NOT EXISTS legal_acceptances (
    id              UUID        PRIMARY KEY,
    organization_id UUID        NOT NULL,
    kind            TEXT        NOT NULL,
    version         INTEGER     NOT NULL,
    principal       TEXT        NOT NULL DEFAULT '',
    client_ip       TEXT        NOT NULL DEFAULT '',
    accepted_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (kind, version) REFERENCES legal_documents (kind, version),
    UNIQUE (organization_id, kind, version)
);
CREATE INDEX IF NOT EXISTS idx_legal_acceptances_kind ON legal_acceptances (kind, version);

-- Version 1 de chaque document, publiée. PROJET À FAIRE RELIRE PAR UN JURISTE.
INSERT INTO legal_documents (kind, version, title_fr, title_en, body_fr, body_en, status, created_by, updated_by, published_by, published_at)
VALUES ('terms_vps', 1,
'Conditions générales de vente des serveurs VPS',
'Terms of sale for VPS servers',
$md$> **Projet à faire relire par un juriste avant ouverture au public.**

# Conditions générales de vente des serveurs VPS

## 1. Objet

Les présentes conditions générales de vente (les « Conditions ») régissent la vente, par betorun, de serveurs privés virtuels (les « VPS ») à ses clients. Toute commande d'un VPS vaut acceptation sans réserve des Conditions en vigueur au jour de la commande, ainsi que de la politique d'usage acceptable de betorun, qui en fait partie intégrante.

## 2. Définitions

- **Client** : la personne, physique ou morale, titulaire d'une organisation sur la console betorun, qui commande un VPS.
- **Organisation** : l'espace du Client sur la console betorun, auquel sont rattachés ses projets, ses VPS et son solde.
- **VPS** : serveur privé virtuel mis à la disposition du Client, avec un accès administrateur (root).
- **Solde** : montant prépayé par le Client sur son Organisation, sur lequel sont débités les prix des VPS et de leurs options.
- **Offre** : configuration de VPS proposée à la vente (processeur, mémoire, disque, emplacement), à un prix mensuel.
- **Date anniversaire** : jour du mois de la commande initiale d'un VPS, à laquelle il est renouvelé chaque mois.
- **Grille de prix** : liste des prix en vigueur des Offres et options, affichée sur la console.

## 3. Commande et prix

3.1. Le Client commande un VPS depuis la console betorun, en choisissant une Offre, un emplacement, un système et une clé SSH.

3.2. Le prix affiché est un prix mensuel, toutes taxes comprises, dans la monnaie de l'Organisation. Il est rappelé au Client avant la validation de la commande ; une commande dont le prix a changé entre l'affichage et la validation est refusée et doit être renouvelée.

3.3. À la commande, le prix du premier mois est débité du Solde. La commande n'est acceptée que si le Solde est suffisant. Le VPS est livré dans les meilleurs délais après le débit ; si sa création échoue, le montant débité est recrédité sur le Solde.

3.4. Chaque VPS est loué par mois complet, à compter de la date de commande.

## 4. Renouvellement

4.1. Le VPS est renouvelé automatiquement chaque mois, à sa Date anniversaire, par débit du Solde.

4.2. Le prix du renouvellement est celui de la Grille de prix en vigueur à la Date anniversaire. Un changement de prix s'applique donc au renouvellement suivant sa publication.

## 5. Solde insuffisant et suspension

5.1. Si le Solde ne permet pas de payer le renouvellement, le VPS est suspendu : il est arrêté et n'est plus accessible.

5.2. Pendant la suspension, les données du VPS sont conservées, tant que la suspension dure et que le VPS n'est pas résilié.

5.3. Après une recharge du Solde suffisante, le renouvellement est débité et le VPS peut être remis en service.

5.4. Les mois pendant lesquels le VPS est resté suspendu ne sont pas facturés.

## 6. Absence de prorata

Les prix sont dus par mois complet. Aucun prorata n'est appliqué, ni à la commande, ni au renouvellement, ni à la résiliation, ni lors de la souscription ou de la résiliation d'une option.

## 7. Résiliation par le Client

7.1. Le Client peut résilier un VPS à tout moment depuis la console. La résiliation prend effet immédiatement : le VPS est arrêté et ne sera plus renouvelé.

7.2. Le mois entamé n'est pas remboursé.

7.3. Les données du VPS résilié sont supprimées ; il appartient au Client de les récupérer avant la résiliation.

## 8. Options

8.1. Le Client peut souscrire des options payantes, notamment l'option snapshot, qui permet de conserver un instantané du disque du VPS.

8.2. Une option est payée par mois, débitée du Solde à la souscription puis renouvelée avec le VPS. Sa résiliation prend effet immédiatement ; le mois entamé n'est pas remboursé, et le snapshot éventuel est supprimé.

## 9. Sauvegardes automatiques

9.1. Lorsque l'Offre en comporte, des sauvegardes automatiques du VPS sont réalisées et peuvent être restaurées depuis la console.

9.2. Ces sauvegardes sont un moyen mis à la disposition du Client et non une garantie de conservation de ses données. Le Client reste responsable de réaliser et de conserver ses propres sauvegardes, en dehors du VPS.

## 10. Disponibilité

betorun met en œuvre les moyens raisonnables pour assurer la disponibilité et le bon fonctionnement des VPS. Il s'agit d'une obligation de moyens : aucun taux de disponibilité n'est garanti. Des interruptions peuvent survenir, notamment pour maintenance ; betorun s'efforce de les annoncer à l'avance sur sa page de statut.

## 11. Responsabilité du Client

11.1. Le Client dispose d'un accès administrateur (root) à son VPS. Il en assure seul l'administration, la mise à jour et la sécurité (mots de passe, clés, pare-feu, logiciels installés).

11.2. Le Client est seul responsable des contenus et des services qu'il héberge ou diffuse au moyen de son VPS, et de leur conformité à la loi.

11.3. Le Client respecte la politique d'usage acceptable de betorun.

## 12. Suspension pour abus

betorun peut suspendre un VPS, sans préavis lorsque la situation l'exige, en cas de manquement à la politique d'usage acceptable, selon la procédure décrite dans celle-ci. La suspension pour abus ne donne lieu à aucun remboursement.

## 13. Données personnelles

Les données personnelles du Client sont traitées conformément à la politique de confidentialité de betorun. Pour les données que le Client traite lui-même sur son VPS, il agit en qualité de responsable de traitement.

## 14. Modification des Conditions

betorun peut modifier les présentes Conditions. Chaque modification fait l'objet d'une nouvelle version, publiée sur la console, que le Client doit accepter pour passer de nouvelles commandes. La version acceptée par le Client reste applicable à ses VPS en cours jusqu'à son acceptation de la nouvelle version.

## 15. Droit applicable et juridiction

[à compléter : droit applicable et juridiction]
$md$,
$md$> **Draft to be reviewed by a lawyer before opening to the public.**

# Terms of sale for VPS servers

## 1. Purpose

These terms of sale (the "Terms") govern the sale, by betorun, of virtual private servers (the "VPS") to its customers. Ordering a VPS means accepting, without reservation, the Terms in force on the day of the order, as well as the betorun acceptable use policy, which forms an integral part of them.

## 2. Definitions

- **Customer**: the person or company holding an organization on the betorun console who orders a VPS.
- **Organization**: the Customer's space on the betorun console, to which its projects, VPS and balance belong.
- **VPS**: a virtual private server made available to the Customer, with administrator (root) access.
- **Balance**: the amount prepaid by the Customer on its Organization, from which the prices of VPS and their options are debited.
- **Offer**: a VPS configuration offered for sale (processor, memory, disk, location) at a monthly price.
- **Anniversary date**: the day of the month of the initial order of a VPS, on which it is renewed every month.
- **Price list**: the list of prices in force for Offers and options, shown on the console.

## 3. Order and price

3.1. The Customer orders a VPS from the betorun console by choosing an Offer, a location, a system and an SSH key.

3.2. The price shown is a monthly price, all taxes included, in the currency of the Organization. It is shown to the Customer before the order is confirmed; an order whose price changed between display and confirmation is refused and must be placed again.

3.3. On ordering, the price of the first month is debited from the Balance. The order is only accepted if the Balance is sufficient. The VPS is delivered as soon as possible after the debit; if its creation fails, the amount debited is credited back to the Balance.

3.4. Each VPS is rented by full month, starting from the order date.

## 4. Renewal

4.1. The VPS is renewed automatically every month, on its Anniversary date, by debit of the Balance.

4.2. The renewal price is the price of the Price list in force on the Anniversary date. A price change therefore applies to the renewal following its publication.

## 5. Insufficient balance and suspension

5.1. If the Balance cannot pay for the renewal, the VPS is suspended: it is stopped and can no longer be reached.

5.2. During the suspension, the data of the VPS is kept, as long as the suspension lasts and the VPS is not terminated.

5.3. Once the Balance has been topped up sufficiently, the renewal is debited and the VPS can be put back into service.

5.4. Months during which the VPS remained suspended are not charged.

## 6. No proration

Prices are due per full month. No proration applies, whether on ordering, renewal or termination, or when subscribing to or cancelling an option.

## 7. Termination by the Customer

7.1. The Customer may terminate a VPS at any time from the console. Termination takes effect immediately: the VPS is stopped and will no longer be renewed.

7.2. The month already started is not refunded.

7.3. The data of the terminated VPS is deleted; the Customer must retrieve it before terminating.

## 8. Options

8.1. The Customer may subscribe to paid options, in particular the snapshot option, which keeps a snapshot of the VPS disk.

8.2. An option is paid monthly, debited from the Balance on subscription and then renewed with the VPS. Its cancellation takes effect immediately; the month already started is not refunded and any snapshot is deleted.

## 9. Automatic backups

9.1. When the Offer includes them, automatic backups of the VPS are taken and can be restored from the console.

9.2. These backups are a means made available to the Customer, not a guarantee that its data will be kept. The Customer remains responsible for making and keeping its own backups, outside the VPS.

## 10. Availability

betorun uses reasonable means to ensure the availability and proper operation of the VPS. This is an obligation of means: no availability rate is guaranteed. Interruptions may occur, in particular for maintenance; betorun endeavours to announce them in advance on its status page.

## 11. Customer responsibility

11.1. The Customer has administrator (root) access to its VPS. It alone manages its administration, updates and security (passwords, keys, firewall, installed software).

11.2. The Customer is solely responsible for the content and services it hosts or distributes by means of its VPS, and for their compliance with the law.

11.3. The Customer complies with the betorun acceptable use policy.

## 12. Suspension for abuse

betorun may suspend a VPS, without notice where the situation requires it, in the event of a breach of the acceptable use policy, following the procedure described in it. A suspension for abuse gives rise to no refund.

## 13. Personal data

The Customer's personal data is processed in accordance with the betorun privacy policy. For the data the Customer itself processes on its VPS, it acts as data controller.

## 14. Changes to the Terms

betorun may change these Terms. Each change results in a new version, published on the console, which the Customer must accept to place new orders. The version accepted by the Customer remains applicable to its current VPS until it accepts the new version.

## 15. Governing law and jurisdiction

[to be completed: governing law and jurisdiction]
$md$,
'published', 'migration', 'migration', 'migration', NOW()),

('aup', 1,
'Politique d''usage acceptable',
'Acceptable use policy',
$md$> **Projet à faire relire par un juriste avant ouverture au public.**

# Politique d'usage acceptable

Cette politique s'applique à tous les services de betorun, et en particulier aux serveurs VPS. Elle fait partie des conditions générales de vente. Le Client en est responsable pour lui-même, ses utilisateurs et toute personne à qui il donne accès à ses services.

## 1. Usages interdits

Il est interdit d'utiliser les services de betorun pour :

1. **Envoyer du spam** : courriers électroniques non sollicités, envoi massif sans consentement des destinataires, listes achetées ou collectées sans accord. Le port 25 sortant (SMTP) des VPS est fermé par défaut ; il peut être ouvert sur demande motivée au support, après vérification.
2. **Hameçonnage** (phishing) : pages ou messages imitant un tiers pour obtenir des identifiants, des coordonnées bancaires ou des données personnelles.
3. **Logiciels malveillants** : héberger, diffuser ou commander des virus, rançongiciels, chevaux de Troie, réseaux de machines compromises (botnets) ou leurs serveurs de commande.
4. **Attaques** : attaques par déni de service (DDoS), balayage de ports ou de vulnérabilités de systèmes tiers sans leur autorisation, tentatives d'intrusion, attaques par force brute.
5. **Minage de cryptomonnaies** : le minage, sous quelque forme que ce soit, est interdit sur les VPS.
6. **Contenus illégaux** : héberger ou diffuser des contenus illicites, notamment pédopornographiques, faisant l'apologie du terrorisme, incitant à la haine ou à la violence.
7. **Atteinte aux droits de tiers** : contrefaçon, diffusion d'œuvres sans l'accord de leurs ayants droit, atteinte à la vie privée, diffamation, usurpation d'identité.
8. **Contournement des limites** : contourner les limites techniques ou commerciales des services (quotas, blocages, mesures de sécurité), ou tenter d'accéder aux ressources d'autres clients ou de la plateforme.

## 2. Procédure en cas d'abus signalé

1. **Signalement** : tout abus peut être signalé à betorun, par un tiers ou détecté par betorun.
2. **Vérification** : betorun vérifie le signalement.
3. **Suspension** : en cas de danger pour des tiers, pour le réseau ou pour la plateforme, le VPS concerné peut être suspendu immédiatement, sans préavis. Dans les autres cas, le Client est invité à corriger la situation dans un délai indiqué.
4. **Information du Client** : le Client est informé par un ticket du support, qui précise les faits reprochés et les mesures prises.
5. **Rétablissement** : le service est rétabli après correction par le Client et vérification par betorun.
6. **Résiliation** : en cas de récidive ou de manquement grave, betorun peut résilier les services concernés, sans remboursement.

## 3. Blocage d'adresse pour spam

Une adresse IP d'un VPS signalée pour envoi de spam peut être bloquée par la plateforme. Le blocage est levé par l'équipe betorun après vérification que l'envoi a cessé et que la cause a été corrigée.

## 4. Coopération avec les autorités

betorun coopère avec les autorités judiciaires et administratives compétentes et leur communique, sur réquisition, les informations que la loi l'oblige à fournir.

## 5. Contact abus

Pour signaler un abus : [adresse de contact abus à compléter]
$md$,
$md$> **Draft to be reviewed by a lawyer before opening to the public.**

# Acceptable use policy

This policy applies to all betorun services, in particular VPS servers. It is part of the terms of sale. The Customer is responsible for it for itself, its users and anyone to whom it gives access to its services.

## 1. Prohibited uses

It is prohibited to use betorun services to:

1. **Send spam**: unsolicited email, mass sending without the recipients' consent, lists bought or collected without agreement. Outbound port 25 (SMTP) of VPS is closed by default; it can be opened upon a justified request to support, after verification.
2. **Phishing**: pages or messages imitating a third party in order to obtain credentials, bank details or personal data.
3. **Malware**: hosting, distributing or controlling viruses, ransomware, trojans, networks of compromised machines (botnets) or their command servers.
4. **Attacks**: denial of service attacks (DDoS), port or vulnerability scanning of third-party systems without their permission, intrusion attempts, brute-force attacks.
5. **Cryptocurrency mining**: mining, in any form, is prohibited on VPS.
6. **Illegal content**: hosting or distributing illegal content, in particular child sexual abuse material, content glorifying terrorism, or inciting hatred or violence.
7. **Infringing third-party rights**: counterfeiting, distributing works without the agreement of their rights holders, invasion of privacy, defamation, identity theft.
8. **Circumventing limits**: circumventing the technical or commercial limits of the services (quotas, blocks, security measures), or attempting to access the resources of other customers or of the platform.

## 2. Procedure when abuse is reported

1. **Report**: any abuse can be reported to betorun by a third party, or detected by betorun.
2. **Verification**: betorun checks the report.
3. **Suspension**: where there is a danger to third parties, to the network or to the platform, the VPS concerned may be suspended immediately, without notice. In other cases, the Customer is asked to correct the situation within a stated time.
4. **Customer information**: the Customer is informed through a support ticket stating the facts and the measures taken.
5. **Restoration**: the service is restored once the Customer has corrected the situation and betorun has checked it.
6. **Termination**: in case of repeated or serious breach, betorun may terminate the services concerned, without refund.

## 3. Address blocking for spam

An IP address of a VPS reported for sending spam may be blocked by the platform. The block is lifted by the betorun team after checking that the sending has stopped and that the cause has been fixed.

## 4. Cooperation with the authorities

betorun cooperates with the competent judicial and administrative authorities and provides them, upon lawful request, with the information the law requires it to provide.

## 5. Abuse contact

To report abuse: [abuse contact address to be completed]
$md$,
'published', 'migration', 'migration', 'migration', NOW())
ON CONFLICT (kind, version) DO NOTHING;
