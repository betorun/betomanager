# Outils de deploy/backup.sh (age, rclone), pour ne rien installer sur l'hôte :
#   docker build -t betorun-backup deploy/backup
FROM alpine:3.23.7
RUN apk add --no-cache age rclone
COPY push.sh /usr/local/bin/push
USER 65534:65534
ENTRYPOINT ["push"]
