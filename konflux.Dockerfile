# Build login page
ARG BRANDING=mta-branding

FROM registry.redhat.io/ubi10/nodejs-22:latest AS frontend
ARG BRANDING
COPY --chown=1001:0 internal/frontend/auth/content/ .
COPY --chown=1001:0 ${BRANDING}/ branding/
RUN npm ci && npm run build

# Go builder to build hub binary
# Copy login page which gets go embedded
FROM registry.redhat.io/ubi10/go-toolset:1.23 AS builder
WORKDIR /workspace
COPY --chown=1001:0 . .
COPY --chown=1001:0 --from=frontend /opt/app-root/src/dist/ internal/frontend/auth/content/dist/
ENV GOEXPERIMENT strictfipsruntime
ENV GOFLAGS=-buildvcs=false
# bin/.build is not being tracked downstream as there is not git tree (.git) directory at build time needed by git describe
RUN make vet && CGO_ENABLED=1 go build -tags json1,strictfipsruntime -o bin/hub github.com/konveyor/tackle2-hub/cmd

# Remove AKS label from Azure target inline
RUN sed -i -e '/Azure\ Kubernetes\ Service/{N;d}' /workspace/hack/build/seed/resources/targets.yaml

# Build tini
# Tini only available in EPEL for now (01/26/2026)
FROM registry.redhat.io/ubi10:latest as tini-builder
COPY --from=builder /workspace/hack/build/tini/ /workspace
RUN dnf install -y cmake make gcc gcc-c++ && dnf -y clean all
WORKDIR /workspace
RUN cmake . && make tini && ./tini --version

FROM brew.registry.redhat.io/rh-osbs/mta-mta-static-report-rhel9:8.0.0 as report

FROM registry.redhat.io/ubi10:latest
RUN mkdir -p /hub && chmod 0777 /hub
ENV HOME=/hub
WORKDIR /hub
ARG VERSION=${BUILD_VERSION}
RUN dnf -y install openssl sqlite openssh-clients subversion git tar vim && dnf -y clean all
RUN echo "hub:x:1001:0:hub:/:/sbin/nologin" >> /etc/passwd

COPY --from=tini-builder /workspace/tini /usr/bin/tini
COPY --from=builder /workspace/bin/hub /usr/local/bin/mta-hub
COPY --from=builder /workspace/LICENSE /licenses/
COPY --from=builder /workspace/hack/build/seed/resources/ /tmp/seed
COPY --from=report  /usr/local/static-report /tmp/analysis/report

RUN echo "${VERSION}" > /etc/hub-build

ENTRYPOINT ["/usr/bin/tini", "--",  "/usr/local/bin/mta-hub"]

LABEL \
        description="Migration Toolkit for Applications - Hub" \
        io.k8s.description="Migration Toolkit for Applications - Hub" \
        io.k8s.display-name="MTA - Hub" \
        io.openshift.maintainer.project="MTA" \
        io.openshift.tags="migration,modernization,mta,tackle,konveyor" \
        summary="Migration Toolkit for Applications - Hub"
