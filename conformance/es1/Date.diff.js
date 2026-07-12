function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var a=new Date(Date.UTC(2020,0,2));var b=new Date(Date.UTC(2020,0,1));check(a.getTime()-b.getTime(),86400000);console.log('PASS');
