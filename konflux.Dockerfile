FROM registry.redhat.io/ubi9/go-toolset:1.23 AS builder
COPY --chown=1001:0 . /workspace
WORKDIR /workspace/hub
ENV GOEXPERIMENT strictfipsruntime
# bin/.build is not being tracked downstream as there is not git tree (.git) directory at build time needed by git describe
RUN make vet && CGO_ENABLED=1 go build -tags json1,strictfipsruntime -o bin/hub github.com/konveyor/tackle2-hub/cmd

# Remove AKS label from Azure target, assumes Azure is the last target listed
RUN sed -i -e '/Azure\ Kubernetes\ Service/,$d' /workspace/hack/build/seed/resources/targets.yaml

FROM brew.registry.redhat.io/rh-osbs/mta-mta-static-report-rhel9:8.0.0 as report

FROM registry.redhat.io/ubi9:latest
# FIX ME : Need CI_VERSION
#ARG VERSION=${CI_VERSION}
RUN dnf -y install openssl sqlite && dnf -y clean all
RUN echo "hub:x:1001:0:hub:/:/sbin/nologin" >> /etc/passwd

COPY --from=builder /workspace/bin/hub /usr/local/bin/mta-hub
COPY --from=builder /workspace/auth/roles.yaml /tmp/roles.yaml
COPY --from=builder /workspace/auth/users.yaml /tmp/users.yaml
COPY --from=builder /workspace/LICENSE /licenses/
COPY --from=builder /workspace/hack/build/seed/resources/ /tmp/seed
COPY --from=report  /usr/local/static-report /tmp/analysis/report

#RUN echo "${VERSION}" > /etc/hub-build

ENTRYPOINT ["/usr/local/bin/mta-hub"]

LABEL \
        description="Migration Toolkit for Applications - Hub" \
        io.k8s.description="Migration Toolkit for Applications - Hub" \
        io.k8s.display-name="MTA - Hub" \
        io.openshift.maintainer.project="MTA" \
        io.openshift.tags="migration,modernization,mta,tackle,konveyor" \
        summary="Migration Toolkit for Applications - Hub"
