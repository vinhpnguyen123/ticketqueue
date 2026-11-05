# syntax=docker/dockerfile:1
#
# One-shot schema migration, run as an ECS task and nothing else.
#
# Why a fourth image rather than baking migrate into the api image: the api
# image would then carry the migrate binary and the whole migrations/ directory
# for the entire life of every task that only ever serves HTTP. This one runs
# for three seconds and exits.

FROM golang:1.26-alpine AS build
# The postgres build tag is required; without it migrate builds with no
# database drivers and fails at runtime with "unknown driver postgres".
RUN CGO_ENABLED=0 go install -tags 'postgres' \
      github.com/golang-migrate/migrate/v4/cmd/migrate@v4.18.1

# alpine, not distroless, and this is the one place that is the right call.
# The connection string arrives as $DATABASE_URL from Secrets Manager, and
# migrate takes it as a -database FLAG. Expanding an environment variable into
# an argument needs a shell. The alternatives are worse: a wrapper binary to
# read one env var, or passing the URL through the task definition in plain
# text where the console would display the password.
#
# The security trade that justified distroless elsewhere does not apply here:
# this container runs for a few seconds, serves no traffic, and listens on
# nothing.
FROM alpine:3.21
RUN adduser -D -u 65532 migrate
COPY --from=build /go/bin/migrate /usr/local/bin/migrate
COPY migrations /migrations
USER migrate

# `up` by default. Override the command in the task to run `down 1`, `force N`,
# or `version`.
ENTRYPOINT ["/bin/sh", "-c", "exec migrate -path=/migrations -database \"$DATABASE_URL\" \"$@\"", "--"]
CMD ["up"]
