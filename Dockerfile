FROM golang:1.27 AS build
WORKDIR /src
COPY . .
RUN go vet ./... && go test ./... \
 && make build && mv up /up \
 && mkdir /data && chown 65534:65534 /data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /up /up
COPY --from=build --chown=65534:65534 /data /data
USER 65534:65534
VOLUME /data
EXPOSE 7493
ENTRYPOINT ["/up"]
CMD ["-config", "/config"]
