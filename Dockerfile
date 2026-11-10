FROM golang:1.27 AS build
WORKDIR /src
COPY . .
RUN go vet ./... && go test ./... \
 && make build && mv up /up \
 && mkdir /data /config && chown 65534:65534 /data /config

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /up /up
COPY --from=build --chown=65534:65534 /data /data
COPY --from=build --chown=65534:65534 /config /config
USER 65534:65534
VOLUME /data /config
EXPOSE 7493
ENTRYPOINT ["/up"]
CMD ["-config", "/config"]
