FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/tapirpost ./cmd/tapirpost
# The final image has no shell to create this with, and the SQLite file needs
# a directory that the unprivileged user can write to.
RUN mkdir /out/data

# Nothing but the binary: it is statically linked and needs no system.
# There are no CA certificates in here either. To verify the TLS certificate
# of a database or cache server, mount a CA bundle and point SSL_CERT_FILE at
# it.
FROM scratch
COPY --from=build /out/tapirpost /tapirpost
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV TAPIRPOST_DB=sqlite:/data/tapirpost.db
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/tapirpost"]
CMD ["serve"]
