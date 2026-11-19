{{/*
Gabarit du pod (métadonnées et spec), partagé par le Deployment (web, worker)
et la CronJob (cron) : mêmes labels (watcher, journaux, métriques), même
sécurité, mêmes variables, même identité.
*/}}
{{- define "standard-app.podTemplate" -}}
metadata:
  labels:
    {{- include "standard-app.labels" . | nindent 4 }}
  annotations:
    # Nouveaux pods quand le contenu du Secret envSecretName change
    # (empreinte calculée par l'API, qui met le Secret à jour AVANT l'upgrade).
    checksum/env: {{ .Values.envChecksum | default "none" | quote }}
spec:
  {{- if eq (include "standard-app.kind" .) "cron" }}
  # Une exécution qui échoue n'est pas relancée dans le même pod : le Job
  # compte l'échec (backoffLimit), la prochaine exécution repart de zéro.
  restartPolicy: Never
  {{- end }}
  serviceAccountName: {{ include "standard-app.serviceAccountName" . }}
  automountServiceAccountToken: {{ .Values.serviceAccount.automountServiceAccountToken | default false }}

  {{- with .Values.image.pullSecrets }}
  imagePullSecrets:
    {{- range . }}
    - name: {{ . }}
    {{- end }}
  {{- end }}

  securityContext:
    {{- toYaml .Values.podSecurityContext | nindent 4 }}

  {{- with .Values.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}

  {{- with .Values.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}

  # Pod anti-affinity : préfère spread sur plusieurs nodes.
  affinity:
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          podAffinityTerm:
            labelSelector:
              matchExpressions:
                - key: app
                  operator: In
                  values:
                    - {{ .Release.Name }}
            topologyKey: kubernetes.io/hostname

  {{- with .Values.waitFor }}
  # Attente du réseau : un nouveau pod n'est autorisé par la politique réseau
  # qu'après quelques secondes ; sans cette attente, l'application qui se
  # connecte dès son démarrage (base, Redis) reçoit « connexion refusée ».
  # Jamais bloquant : après 60 s par adresse, l'application démarre quand même.
  initContainers:
    - name: wait-network
      image: {{ .image | quote }}
      imagePullPolicy: IfNotPresent
      command:
        - sh
        - -c
        - |
          for t in $WAIT_FOR; do
            h="${t%:*}"; p="${t##*:}"; i=0
            until nc -z -w 2 "$h" "$p"; do
              i=$((i+1))
              if [ "$i" -ge 60 ]; then echo "$t injoignable après 60 s : démarrage quand même"; break; fi
              sleep 1
            done
          done
      env:
        - name: WAIT_FOR
          value: {{ .targets | quote }}
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        runAsNonRoot: true
        runAsUser: 65534
        capabilities:
          drop: ["ALL"]
      resources:
        requests:
          cpu: 10m
          memory: 8Mi
        limits:
          cpu: 50m
          memory: 16Mi
  {{- end }}

  containers:
    - name: app
      image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
      imagePullPolicy: {{ .Values.image.pullPolicy | default "IfNotPresent" }}

      {{- with .Values.command }}
      # Commande de démarrage personnalisée (remplace l'ENTRYPOINT de l'image).
      command:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.args }}
      args:
        {{- toYaml . | nindent 8 }}
      {{- end }}

      securityContext:
        {{- toYaml .Values.containerSecurityContext | nindent 8 }}

      {{- if eq (include "standard-app.kind" .) "web" }}
      ports:
        - name: http
          containerPort: {{ .Values.service.targetPort | default 8080 }}
          protocol: {{ .Values.service.protocol | default "TCP" }}
      {{- else if eq (include "standard-app.kind" .) "service" }}
      ports:
        - name: tcp
          containerPort: {{ .Values.service.targetPort }}
          protocol: TCP
      {{- end }}

      # Env depuis le Secret (buildé par EnsureSecret côté Go).
      {{- if .Values.envSecretName }}
      envFrom:
        - secretRef:
            name: {{ .Values.envSecretName }}
      {{- end }}

      {{- if or .Values.envVars .Values.betorunIdentity.enabled }}
      env:
        {{- range $key, $val := .Values.envVars }}
        - name: {{ $key }}
          value: {{ $val | quote }}
        {{- end }}
        {{- if .Values.betorunIdentity.enabled }}
        # Identité du workload : jeton Kubernetes à échanger auprès du STS
        # betorun (grant token-exchange) contre un jeton du compte de service.
        - name: BETORUN_TOKEN_FILE
          value: /var/run/secrets/betorun/token
        - name: BETORUN_STS_TOKEN_URI
          value: {{ .Values.betorunIdentity.tokenURI | quote }}
        - name: BETORUN_SERVICE_ACCOUNT
          value: {{ .Values.betorunIdentity.serviceAccount | quote }}
        {{- end }}
      {{- end }}

      # Probes : startup → readiness → liveness, chacune conditionnelle.
      {{- if .Values.probes.startup.enabled }}
      startupProbe:
        {{- omit .Values.probes.startup "enabled" | toYaml | nindent 8 }}
      {{- end }}
      {{- if .Values.probes.readiness.enabled }}
      readinessProbe:
        {{- omit .Values.probes.readiness "enabled" | toYaml | nindent 8 }}
      {{- end }}
      {{- if .Values.probes.liveness.enabled }}
      livenessProbe:
        {{- omit .Values.probes.liveness "enabled" | toYaml | nindent 8 }}
      {{- end }}

      resources:
        {{- toYaml .Values.resources | nindent 8 }}

      volumeMounts:
        # tmpfs : /tmp et /var/run en writable malgré readOnlyRootFilesystem.
        {{- if .Values.tmpfs.enabled }}
        {{- range .Values.tmpfs.mounts }}
        - name: {{ .name }}
          mountPath: {{ .path }}
        {{- end }}
        {{- end }}

        {{- if .Values.persistence.enabled }}
        - name: storage
          mountPath: {{ .Values.persistence.mountPath | default "/data" }}
        {{- end }}

        {{- if .Values.betorunIdentity.enabled }}
        - name: betorun-token
          mountPath: /var/run/secrets/betorun
          readOnly: true
        {{- end }}

  volumes:
    {{- if .Values.tmpfs.enabled }}
    {{- range .Values.tmpfs.mounts }}
    - name: {{ .name }}
      emptyDir:
        medium: Memory
        sizeLimit: {{ .sizeLimit | default "16Mi" }}
    {{- end }}
    {{- end }}

    {{- if .Values.persistence.enabled }}
    - name: storage
      persistentVolumeClaim:
        claimName: {{ .Release.Name }}-pvc
    {{- end }}

    {{- if .Values.betorunIdentity.enabled }}
    # Jeton projeté : audience = STS betorun, renouvelé par le kubelet.
    - name: betorun-token
      projected:
        sources:
          - serviceAccountToken:
              path: token
              audience: {{ required "betorunIdentity.audience" .Values.betorunIdentity.audience | quote }}
              expirationSeconds: 3600
    {{- end }}
{{- end -}}
