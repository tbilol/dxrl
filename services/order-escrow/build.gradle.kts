import org.springframework.boot.gradle.plugin.SpringBootPlugin

// order-escrow build. Targets Java 17 bytecode (`release = 17`) and runs on any
// JDK 17+. Use the wrapper (./gradlew), which downloads the pinned Gradle.

plugins {
    java
    alias(libs.plugins.spring.boot)
}

group = "uz.agrogram"
version = "0.1.0"

// ---- Generated contract code (back/gen/java) ---------------------------------
//
// `make gen` (back/tools/codegen.sh, the shared tooling) generates Java for
// every proto package into back/gen/java: git-ignored, package prefix `uz`, so
// agrogram.catalog.v1 becomes uz.agrogram.catalog.v1. A Java service compiles
// the packages it uses as its own `contracts` source set:
//
//   - only the listed packages are compiled. The list must be closed under
//     proto imports: orderescrow and catalog import common and events, events
//     imports common. Other services' APIs stay out of this jar;
//   - buf.validate.* is not generated: build.buf:protovalidate provides it;
//   - the gencode compiles without this project's strict lint flags;
//   - main and test depend on it like on a library, and bootJar packs its
//     classes next to the service's own.
//
// A future Java service copies this block and changes `contractPackages`.
val genJavaDir: File = rootDir.resolve("../../gen/java").normalize()
val contractPackages = listOf("common", "events", "catalog", "orderescrow")

val contracts: SourceSet = sourceSets.create("contracts") {
    java {
        setSrcDirs(listOf(genJavaDir))
        contractPackages.forEach { include("uz/agrogram/$it/**") }
    }
}

sourceSets {
    main {
        compileClasspath += contracts.output
        runtimeClasspath += contracts.output
    }
    test {
        compileClasspath += contracts.output
        runtimeClasspath += contracts.output
    }
}

configurations.implementation {
    extendsFrom(configurations[contracts.implementationConfigurationName])
}

dependencies {
    // Platforms. Gradle picks the highest version any of them asks for, so the
    // protobuf and gRPC BOMs (pinned to the gencode) override Spring Boot's.
    val contractsImplementation = contracts.implementationConfigurationName
    contractsImplementation(platform(SpringBootPlugin.BOM_COORDINATES))
    contractsImplementation(platform(libs.protobuf.bom))
    contractsImplementation(platform(libs.grpc.bom))

    // What the generated messages and stubs need.
    contractsImplementation("com.google.protobuf:protobuf-java")
    contractsImplementation("io.grpc:grpc-protobuf")
    contractsImplementation("io.grpc:grpc-stub")
    contractsImplementation(libs.protovalidate)

    // gRPC server and the catalog client (Netty), health and reflection.
    implementation("io.grpc:grpc-netty-shaded")
    implementation("io.grpc:grpc-services")

    // HTTP /healthz and /readyz, Postgres (JDBC + HikariCP), migrations, Kafka.
    implementation("org.springframework.boot:spring-boot-starter-webmvc")
    implementation("org.springframework.boot:spring-boot-starter-jdbc")
    implementation("org.flywaydb:flyway-core")
    implementation("org.flywaydb:flyway-database-postgresql")
    implementation("org.apache.kafka:kafka-clients")
    runtimeOnly("org.postgresql:postgresql")

    testImplementation("org.springframework.boot:spring-boot-starter-test")
    testImplementation("io.grpc:grpc-inprocess")
    testImplementation("io.grpc:grpc-testing")
    testRuntimeOnly("org.junit.platform:junit-platform-launcher")
}

tasks.withType<JavaCompile>().configureEach {
    options.release = 17
    options.encoding = "UTF-8"
}

// The service's own code: lint as errors. The categories are listed rather
// than `all` so that newer JDKs (which add categories) build the same way.
val strictLint = listOf(
    "-Xlint:cast,deprecation,divzero,empty,fallthrough,finally,overrides,rawtypes,static,unchecked,varargs",
    "-Werror",
)
tasks.named<JavaCompile>("compileJava") { options.compilerArgs.addAll(strictLint) }
tasks.named<JavaCompile>("compileTestJava") { options.compilerArgs.addAll(strictLint) }

tasks.named<JavaCompile>("compileContractsJava") {
    options.compilerArgs.add("-Xlint:none")
    val dir = genJavaDir
    doFirst {
        if (!dir.resolve("uz/agrogram/orderescrow/v1").isDirectory) {
            throw GradleException(
                "$dir has no generated order-escrow code. Generate it first: make gen " +
                    "(back/tools/codegen.sh generate), or from back/: make proto-gen-local.",
            )
        }
    }
}

springBoot {
    mainClass = "uz.agrogram.orderescrow.OrderEscrowApplication"
}

tasks.bootJar {
    archiveFileName = "order-escrow.jar"
}

// Only the executable boot jar is built.
tasks.jar {
    enabled = false
}

// Unit tests need no infrastructure. Tests tagged "integration" need Postgres
// (ORDER_ESCROW_TEST_DATABASE_URL) and run only in integrationTest.
tasks.test {
    useJUnitPlatform {
        excludeTags("integration")
    }
}

tasks.register<Test>("integrationTest") {
    description = "Runs the tests tagged 'integration' against ORDER_ESCROW_TEST_DATABASE_URL."
    group = LifecycleBasePlugin.VERIFICATION_GROUP
    testClassesDirs = sourceSets.test.get().output.classesDirs
    classpath = sourceSets.test.get().runtimeClasspath
    useJUnitPlatform {
        includeTags("integration")
    }
    shouldRunAfter(tasks.test)
    // They depend on the database, not only on inputs Gradle can see.
    outputs.upToDateWhen { false }
}

tasks.withType<Test>().configureEach {
    testLogging {
        events("failed", "skipped")
        exceptionFormat = org.gradle.api.tasks.testing.logging.TestExceptionFormat.FULL
    }
}
