FROM golang:1.16.4 as builder
ADD . /src
WORKDIR /src/otelcolbuilder/
RUN make install
RUN make build

FROM alpine:3.13 as certs
RUN apk --update add ca-certificates

FROM scratch
ARG BUILD_TAG=latest
ENV TAG $BUILD_TAG
ARG USER_UID=10001
USER ${USER_UID}

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /src/otelcolbuilder/cmd/otelcol-sumo /otelcol-sumo
EXPOSE 55680 55679
ENTRYPOINT ["/otelcol-sumo"]
CMD ["--config", "/etc/otel/config.yaml"]