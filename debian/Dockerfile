FROM debian:jessie

# persistent / runtime deps
RUN apt-get update && apt-get install -y ca-certificates curl librecode0 libsqlite3-0 libxml2 ldap-utils libldap-2.4-2 libdb4.6 --no-install-recommends && rm -r /var/lib/apt/lists/*

# phpize deps
RUN apt-get update && apt-get install -y autoconf file g++ gcc libc-dev make pkg-config re2c --no-install-recommends && rm -r /var/lib/apt/lists/*

RUN find / | grep ldap