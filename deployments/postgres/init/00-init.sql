-- vornik PostgreSQL initialization script
-- Runs automatically on first container start
-- 
-- This script creates the application schema and sets up
-- the foundation for the vornik persistence layer.

-- Create application schema (optional, for organization)
CREATE SCHEMA IF NOT EXISTS vornik;

-- Grant permissions to the application user
GRANT ALL PRIVILEGES ON SCHEMA vornik TO vornik;

-- Set default search path for convenience
ALTER DATABASE vornik SET search_path TO vornik, public;

-- The actual tables will be created by vornik migrations
-- This file just sets up the baseline

-- Log initialization
DO $$
BEGIN
    RAISE NOTICE 'vornik PostgreSQL database initialized';
    RAISE NOTICE 'Schema: vornik';
    RAISE NOTICE 'Ready for application migrations';
END $$;