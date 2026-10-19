// SPDX-License-Identifier: MIT
import React from "react";
import Link from "@docusaurus/Link";
import Layout from "@theme/Layout";
import styles from "./index.module.css";

const surfaces = [
  {
    title: "Onboarding guide",
    to: "/onboarding",
    text: "Get an app key, make your first authenticated call, and learn the platform conventions in one linear walkthrough.",
  },
  {
    title: "Webhooks",
    to: "/webhooks",
    text: "Register delivery targets on your own user-scoped Svix app, verify signatures, and read the platform event catalog.",
  },
  {
    title: "Mini-apps",
    to: "/mini-apps",
    text: "Register, submit, and launch sandboxed mini-apps: immutable slugs, permission manifests, and HMAC launch tokens.",
  },
];

export default function Home() {
  return (
    <Layout title="Sutra Developers" description="Build on the Sutra platform">
      <main className={styles.hero}>
        <h1>Build on Sutra</h1>
        <p>
          Mini-apps, webhooks, and a REST API — one source of truth (
          <code>docs/openapi.yaml</code>, CI-gated), one onboarding guide.
        </p>
        <div className={styles.cards}>
          {surfaces.map((s) => (
            <Link key={s.to} className={styles.card} to={s.to}>
              <h3>{s.title}</h3>
              <p>{s.text}</p>
            </Link>
          ))}
        </div>
      </main>
    </Layout>
  );
}
