-- Create sites table
CREATE TABLE IF NOT EXISTS sites (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subdomain   TEXT NOT NULL UNIQUE,
    active      BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_sites_subdomain ON sites(subdomain);

-- Seed testing tenants
-- We hardcode the UUIDs so we know exactly what directories to create in MinIO
INSERT INTO sites (id, subdomain, active) VALUES
('550e8400-e29b-41d4-a716-446655440000', 'tenant-a', true),
('6ba7b810-9dad-11d1-80b4-00c04fd430c8', 'tenant-b', true)
ON CONFLICT (subdomain) DO NOTHING;
