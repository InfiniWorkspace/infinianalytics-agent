# The agent as a container, for hosts where installing a service is not an
# option. It still reports the HOST, not the container:
#
#   docker run -d --name infinianalytics-agent --restart unless-stopped \
#     --network host \
#     -v /var/run/docker.sock:/var/run/docker.sock:ro \
#     -v /:/hostfs:ro \
#     -v infinianalytics-agent:/state \
#     -e IA_AGENT_ENROLL_CODE=XXXX-XXXX-XXXX -e IA_AGENT_URL=https://api.analytics.infini.es \
#     ghcr.io/infiniworkspace/infinianalytics-agent:latest
#
# /proc is not namespaced for CPU, memory, disk I/O, uptime or the boot id, so
# those are the host's as-is; --network host makes the network counters the
# host's too; /hostfs gives the host's disks, machine id, hostname and OS
# release; the socket gives its containers. The code is used once - the key it
# is traded for lives in the /state volume; a new code (a reinstall) enrolls again
# in place and keeps the spool. -e IA_AGENT_DISKS=false and/or
# -e IA_AGENT_DOCKER=false leave those modules out (drop the socket mount too).
FROM golang:1.27 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/infinianalytics-agent .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/infinianalytics-agent /infinianalytics-agent
ENV IA_AGENT_CONFIG=/state/agent.env \
    IA_AGENT_HOST_ROOT=/hostfs \
    GOMAXPROCS=1 \
    GOMEMLIMIT=40MiB
VOLUME ["/state"]
ENTRYPOINT ["/infinianalytics-agent"]
CMD ["run"]
