// PM2 process definition for the RobloxKit gateway VPS deployment.
//
// Every field below is part of the single-instance deployment contract, not
// a stylistic choice: the Bridge hub registry, the rate limiter buckets, and
// the live WSS sessions are process-local state, so cluster mode or a second
// instance would silently split routing state across processes.
// internal/appconfig/production_test.go (ValidateEcosystem) enforces this
// file — change the two together.
module.exports = {
  apps: [
    {
      name: "robloxkit-server",
      script: "./bin/robloxkit-server",
      exec_mode: "fork",
      instances: 1,
      autorestart: true,
      restart_delay: 5000,
      kill_timeout: 40000, // must exceed the server's 30s drain budget
      max_restarts: 10,
      max_memory_restart: "512M",
      time: true,
    },
  ],
};
