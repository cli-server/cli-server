import pg from "pg";
import { config } from "../config.js";

export const pool = new pg.Pool({
  connectionString: config.databaseUrl,
  max: 10,
});

pool.on("error", (err) => {
  console.error("Unexpected PostgreSQL pool error:", err);
});
