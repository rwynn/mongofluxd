FROM alpine:edge as builder
RUN apk add --no-cache build-base git go
RUN mkdir /app
WORKDIR /app
COPY . .
RUN env GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -v -o build/mongofluxd

FROM alpine:edge
RUN apk add --no-cache ca-certificates
ARG BUILD_DATE
ARG VCS_REF
ARG VSC_URL
ARG BUILD_VERSION
LABEL org.label-schema.build-date=$BUILD_DATE \
      org.label-schema.vcs-url=$VSC_URL \
      org.label-schema.vcs-ref=$VCS_REF \
      org.label-schema.schema-version=$BUILD_VERSION
ENTRYPOINT ["/bin/mongofluxd"]
COPY --from=builder /app/build/mongofluxd /bin/mongofluxd
