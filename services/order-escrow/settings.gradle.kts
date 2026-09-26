// order-escrow: the escrow transaction (tranzaksiya) service, AGROGRAM's first
// Java service. A standalone Gradle build: run ./gradlew from this directory
// (back/services/order-escrow). The generated contract code comes from
// back/gen/java, see build.gradle.kts.

pluginManagement {
    repositories {
        gradlePluginPortal()
        mavenCentral()
    }
}

dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        mavenCentral()
    }
}

rootProject.name = "order-escrow"
