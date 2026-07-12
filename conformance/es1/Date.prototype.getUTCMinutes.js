function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var d=new Date(Date.UTC(2020,5,15,10,30,45,500)); check(d.getUTCMinutes(),30);console.log('PASS');
